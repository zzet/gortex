package main

import (
	"encoding/json"
	"testing"
)

func TestMixedBackgroundReadsOnlyFinalRetainedStatus(t *testing.T) {
	for _, mode := range []string{"latest", "last_refused", "last_missing_payload", "malformed_tail", "structured_mismatch", "integer_mismatch", "decimal_mismatch", "equivalent_numbers"} {
		t.Run(mode, func(t *testing.T) {
			_, body := mixedBackgroundFixture(t)
			var decoded map[string]any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			if mode == "integer_mismatch" {
				decoded["commit_seq"] = json.Number("9007199254740992")
				var err error
				body, err = json.Marshal(decoded)
				if err != nil {
					t.Fatal(err)
				}
			}
			if mode == "decimal_mismatch" {
				decoded["phase_timing"].(map[string]any)["patch_accounting"].(map[string]any)["phases_ms"].(map[string]any)["apply"] = json.Number("5000000000000.000001")
				var err error
				body, err = json.Marshal(decoded)
				if err != nil {
					t.Fatal(err)
				}
			}
			payload := map[string]any{"content": []any{map[string]any{"type": "text", "text": string(body)}}}
			first := map[string]any{"event": "rpc", "purpose": "status", "tool": "change.receipt", "wire_id": 1, "payload": payload}
			raw, err := json.Marshal(first)
			if err != nil {
				t.Fatal(err)
			}
			tail := map[string]any{"event": "rpc", "purpose": "status", "tool": "change.receipt", "wire_id": 2, "payload": payload}
			switch mode {
			case "integer_mismatch":
				decoded["commit_seq"] = json.Number("9007199254740993")
				payload["structuredContent"] = decoded
			case "decimal_mismatch":
				decoded["phase_timing"].(map[string]any)["patch_accounting"].(map[string]any)["phases_ms"].(map[string]any)["apply"] = json.Number("5000000000000.000002")
				payload["structuredContent"] = decoded
			case "equivalent_numbers":
				decoded["commit_seq"] = json.Number("1.0")
				payload["structuredContent"] = decoded
			case "last_refused":
				tail["error"] = "superseded"
			case "last_missing_payload":
				delete(tail, "payload")
			case "structured_mismatch":
				payload["structuredContent"] = map[string]any{"receipt": "foreign"}
			}
			end, err := json.Marshal(tail)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "malformed_tail" {
				end = []byte(`{"event":`)
			}
			raw = append(append(raw, '\n'), end...)
			got, err := latestMixedBackgroundStatus(raw)
			if (err == nil) != (mode == "latest" || mode == "equivalent_numbers") {
				t.Fatalf("mode=%s error=%v body=%s", mode, err, got)
			}
		})
	}
}
