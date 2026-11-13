package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEditLoopSeparateWinnerPreexistingOriginalBackgroundIsNotDirectAcceptance(t *testing.T) {
	for _, mode := range []string{"separate_winner", "direct_accepted_original", "foreign_winner", "foreign_refusal_owner"} {
		t.Run(mode, func(t *testing.T) {
			r, raw, first := coalescedEditLoopFixture(t)
			proof := first["coalesced_functional_receipt"].(map[string]any)
			status := proof["status"].(map[string]any)
			status["background_refusal"] = "preexisting_path_work"
			for _, key := range []string{"binding_predicate", "background_stage", "accepted_attempt_stage"} {
				status[key] = ""
			}
			reason := strings.ReplaceAll(proof["original_accounting_error"].(string), "serving_attempt_binding_mismatch", "preexisting_path_work")
			proof["original_accounting_error"] = reason
			first["accounting"].(map[string]any)["reasons"] = strings.Split(strings.TrimPrefix(reason, "A/P/B accounting unavailable: "), "; ")
			switch mode {
			case "direct_accepted_original":
				status["applied_generation"], status["physical_unavailable_observed"], status["coalesced_absent_observed"] = 3, false, true
				first["accounting"].(map[string]any)["applied_generation"] = 3
				first["accounting"].(map[string]any)["physical"] = proof["serving_physical"]
			case "foreign_winner":
				proof["winner"].(map[string]any)["ticket"] = 999
			case "foreign_refusal_owner":
				status["background_requested_generation"] = 999
			}
			encoded, err := json.Marshal(raw)
			if err != nil {
				t.Fatal(err)
			}
			r.Phases[0].EditProbe = encoded
			_, err = decodeEditLoopWire(r.Phases[0])
			if (err == nil) != (mode == "separate_winner") {
				t.Fatalf("mode=%s error=%v", mode, err)
			}
		})
	}
}
