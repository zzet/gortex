package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func searchACKFixture() map[string]any {
	return map[string]any{"ack_mode": "local", "content_sha256": strings.Repeat("a", 64), "readback_exact": true, "graph_status_hint": "file current; recorded graph status: pending", "file_view": map[string]any{
		"state": "projected", "delta_state": "available", "counts": map[string]any{"before": 2, "after": 2, "added": 1, "removed": 1, "changed": 0, "total_changes": 2},
		"symbols": []any{
			map[string]any{"change": "added", "id": "fixture/sample.go::Now", "kind": "function", "start_line": 3, "end_line": 3, "signature": "func Now()"},
			map[string]any{"change": "removed", "id": "fixture/sample.go::Before", "kind": "function", "start_line": 3, "end_line": 3, "signature": "func Before()"},
		}, "truncated": false, "syntax_health": map[string]any{"state": "checked", "checked": true, "errors": 0, "language": "go"},
	}}
}

// Round-trip the artifact shape, replacing the prior ACK map completely.
// json.Unmarshal otherwise merges object keys and would conceal a deleted key.
func addSearchACKFixture(t *testing.T, record *editProbeRecord, proof map[string]any) {
	t.Helper()
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	var row map[string]json.RawMessage
	if err := json.Unmarshal(data, &row); err != nil {
		t.Fatal(err)
	}
	row["search_ack"], err = json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(row)
	if err != nil {
		t.Fatal(err)
	}
	record.SearchACK = nil
	if err := json.Unmarshal(data, record); err != nil {
		t.Fatal(err)
	}
}

func TestSearchReadbackCensusRequiresObservedLocalObjectACK(t *testing.T) {
	for _, fault := range []string{"valid", "historical_unavailable", "missing", "full", "scalar_view", "short_hash", "missing_hint", "missing_syntax"} {
		t.Run(fault, func(t *testing.T) {
			plan, records := completeSearchReadbackCensus(t)
			for i := range records {
				proof := searchACKFixture()
				if i == 0 {
					switch fault {
					case "historical_unavailable":
						proof["readback_exact"] = false
						proof["file_view"] = map[string]any{"state": "unavailable", "reason": "projection_not_live", "historical": true, "delta_state": "unavailable", "delta_reason": "projection_not_live", "counts": nil, "symbols": nil, "truncated": false, "syntax_health": map[string]any{"state": "unknown", "checked": false, "errors": nil}}
					case "missing":
						proof = nil
					case "full":
						proof["ack_mode"] = "full"
					case "scalar_view":
						proof["file_view"] = "projected"
					case "short_hash":
						proof["content_sha256"] = "aa"
					case "missing_hint":
						delete(proof, "graph_status_hint")
					case "missing_syntax":
						delete(proof["file_view"].(map[string]any), "syntax_health")
					}
				}
				addSearchACKFixture(t, &records[i], proof)
			}
			got := summarizeSearchReadbacks(plan, records)
			if got.Pass != (fault == "valid" || fault == "historical_unavailable") {
				t.Fatalf("actual local/object ACK requirement misclassified %s: %+v", fault, got)
			}
		})
	}
}
