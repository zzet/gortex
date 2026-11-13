package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func coalescedEditLoopFixture(t *testing.T) (report, map[string]any, map[string]any) {
	t.Helper()
	r, raw, first := supersededEditLoopFixture(t)
	data, err := os.ReadFile("../receiptproof/testdata/coalesced.json")
	if err != nil {
		t.Fatal(err)
	}
	var proof map[string]any
	if err := json.Unmarshal(data, &proof); err != nil {
		t.Fatal(err)
	}
	delete(first, "superseded_receipt")
	first["outcome"], first["coalesced_functional_receipt"] = "qualified_coalesced_functional", proof
	first["accounting"].(map[string]any)["applied_generation"] = 4
	first["accounting"].(map[string]any)["reasons"] = strings.Split(strings.TrimPrefix(proof["original_accounting_error"].(string), "A/P/B accounting unavailable: "), "; ")
	first["graph_terminal"], first["stale_claimed_fresh"] = true, false
	first["evidence_bytes"], first["evidence_limit_bytes"] = 1000, 8388608
	first["t_readback_ms"] = map[string]any{"value": 50, "unit": "ms"}
	first["first_correct_ms"] = map[string]any{}
	for _, name := range []string{"source", "summary", "editing_context", "callers", "call_chain", "search_visible", "text"} {
		first["first_correct_ms"].(map[string]any)[name] = map[string]any{"value": 1, "unit": "ms"}
	}
	for _, x := range first["readbacks"].([]any) {
		x.(map[string]any)["latency_ms"] = map[string]any{"value": 1, "unit": "ms"}
	}
	return r, raw, first
}

func TestEditLoopQualifiedCoalescedFunctionalChecksIndependently(t *testing.T) {
	for _, mode := range []string{"qualified", "missing_proof", "wrong_rpc", "unknown_scope", "accepted_original", "wrong_ACK", "incorrect_read", "missing_oracle", "false_fresh", "capped", "wrong_winner", "invented_background", "unknown_B_phase", "empty_B_phase", "unknown_mismatch", "empty_mismatch"} {
		t.Run(mode, func(t *testing.T) {
			r, raw, first := coalescedEditLoopFixture(t)
			proof := first["coalesced_functional_receipt"].(map[string]any)
			switch mode {
			case "missing_proof":
				delete(first, "coalesced_functional_receipt")
			case "wrong_rpc":
				proof["status"].(map[string]any)["acknowledgement_rpc_id"] = "foreign"
			case "unknown_scope":
				delete(raw, "physical_write_scope")
			case "accepted_original":
				first["accounting"].(map[string]any)["physical"] = map[string]any{"wall_ns": 1}
			case "wrong_ACK":
				first["ack_disk_status"] = "pending"
			case "incorrect_read":
				first["readbacks"].([]any)[0].(map[string]any)["correct"] = false
			case "missing_oracle":
				delete(first["first_correct_ms"].(map[string]any), "call_chain")
			case "false_fresh":
				first["stale_claimed_fresh"] = true
			case "capped":
				first["outcome"] = "harness_evidence_incomplete"
			case "wrong_winner":
				proof["winner"].(map[string]any)["ticket"] = 9
			case "unknown_B_phase", "empty_B_phase", "unknown_mismatch", "empty_mismatch":
				suffix := map[string]string{"unknown_B_phase": "missing B phase unknown", "empty_B_phase": "missing B phase ", "unknown_mismatch": "B/P phase mismatch: unknown", "empty_mismatch": "B/P phase mismatch: "}[mode]
				proof["original_accounting_error"] = proof["original_accounting_error"].(string) + "; " + suffix
				ac := first["accounting"].(map[string]any)
				ac["reasons"] = append(ac["reasons"].([]string), suffix)
			case "invented_background":
				first["accounting"].(map[string]any)["requested_background"] = map[string]any{"wall_ns": 1}
			}
			encoded, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			r.Phases[0].EditProbe = encoded
			_, err = decodeEditLoopWire(r.Phases[0])
			if (err == nil) != (mode == "qualified") {
				t.Fatalf("mode=%s error=%v", mode, err)
			}
		})
	}
}
