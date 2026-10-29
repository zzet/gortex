package main

import (
	"encoding/json"
	"testing"
)

func TestEditProbeAcceptsOnlyExplicitMeasuredNativeHandoffCoverage(t *testing.T) {
	for _, name := range []string{"chain", "missing_count", "zero_count", "one_count", "unknown", "single_contradiction"} {
		t.Run(name, func(t *testing.T) {
			body := editProbeRepairAccountingFixture()
			body["applied_generation"] = json.Number("3")
			winner := body["coalesced_publication"].(map[string]any)
			winner["applied_generation"] = json.Number("3")
			winner["publication_phases"].(map[string]any)["ticket"] = json.Number("3")
			winner["timings"].(map[string]any)["phase_timing"].(map[string]any)["patch_accounting"].(map[string]any)["attempt_ticket"] = json.Number("3")
			background := body["requested_background"].(map[string]any)
			background["applied_generation"] = json.Number("3")
			background["coverage"] = "successful_pre_callback_native_handoffs"
			background["observed_native_handoffs"] = json.Number("2")
			switch name {
			case "missing_count":
				delete(background, "observed_native_handoffs")
			case "zero_count":
				background["observed_native_handoffs"] = json.Number("0")
			case "one_count":
				background["observed_native_handoffs"] = json.Number("1")
			case "unknown":
				background["coverage"] = "assumed_native_chain"
			case "single_contradiction":
				background["coverage"] = "one_canceled_timer_one_native_successor"
			}
			got := checkEditProbeAccounting(body, "receipt-a")
			if (got.State == "observed_single_window_checks_pass") != (name == "chain") {
				t.Fatalf("coverage%s: %+v", name, got)
			}
		})
	}
}

func TestEditProbeOwnedBackgroundRefusalIsTerminalFailedCoverageNotMissingAsZero(t *testing.T) {
	for _, name := range []string{"observed", "missing", "unknown_reason", "foreign_publication", "foreign_requested", "foreign_last", "missing_count", "wrong_source", "contradictory_B"} {
		t.Run(name, func(t *testing.T) {
			body := editProbeRepairAccountingFixture()
			delete(body, "requested_background")
			diagnostic := map[string]any{"scope": "requested_background_observation", "reason": "timer_callback_already_started", "publication_key": "original", "requested_generation": json.Number("1"), "last_observed_generation": json.Number("2"), "observed_native_handoffs": json.Number("1")}
			body["requested_background_unavailable"] = diagnostic
			switch name {
			case "missing":
				delete(body, "requested_background_unavailable")
			case "unknown_reason":
				diagnostic["reason"] = "maybe_busy"
			case "foreign_publication":
				diagnostic["publication_key"] = "receipt-a"
			case "foreign_requested":
				diagnostic["requested_generation"] = json.Number("2")
			case "foreign_last":
				diagnostic["last_observed_generation"] = json.Number("3")
			case "missing_count":
				delete(diagnostic, "observed_native_handoffs")
			case "wrong_source":
				body["publication_phases"].(map[string]any)["source"] = "canonical_coalesced_attempt"
			case "contradictory_B":
				body["requested_background"] = map[string]any{}
			}
			got := checkEditProbeAccounting(body, "receipt-a")
			if got.State == "observed_single_window_checks_pass" || got.B != nil {
				t.Fatalf("unavailable B became coverage: %+v", got)
			}
			if (got.State == "terminal_unavailable_background") != (name == "observed") {
				t.Fatalf("refusal%s: %+v", name, got)
			}
			if editProbeFinished(true, true, true, got) != (name == "observed") {
				t.Fatalf("unowned/missing refusal stopped collection: %+v", got)
			}
			if editProbeFinished(false, true, true, got) || editProbeFinished(true, false, true, got) {
				t.Fatal("refusal bypassed graph/oracle completion")
			}
		})
	}
}
