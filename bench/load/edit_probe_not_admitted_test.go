package main

import "testing"

// The wire cases the restore test's fake server does not send. Only the
// product's exact no-mutation wording, or a backoff after refusals LOAD
// already re-sends, makes an edit's outcome known.
func TestEditProbeNotAdmittedNeedsExactNoMutationWording(t *testing.T) {
	refused := func(message string) Attempt {
		return Attempt{Code: -32000, Error: "RPC -32000: " + message, Outcome: "error"}
	}
	loading := Attempt{Code: -32000, Error: "RPC -32000: repository loading", ErrorData: map[string]any{"error_code": "repo_loading", "retryable": true}, Outcome: "retryable"}
	backoff := Attempt{Error: busyBackoffEnded + ": context deadline exceeded", Outcome: "product_deadline"}
	busy := Attempt{Code: -32002, Error: "RPC -32002: MCP dispatcher is busy", ErrorData: map[string]any{"outcome": "server_busy", "retryable": true, "delivery_unknown": false}, Outcome: "busy"}
	// A request sent after busy refusals, still without a reply when the
	// deadline ended: it may have been admitted.
	outstanding := Attempt{Error: "context deadline exceeded", DurationMS: 24000, Outcome: "phase_censored"}
	late := refused("view_building: checkout \"c\" is not fully routed yet")
	late.Outcome = "product_deadline"
	for _, tc := range []struct {
		name     string
		attempts []Attempt
		want     string
	}{
		{"byte_admission", []Attempt{refused("view_read_only: byte mutation admission refused; no files were changed: lease moved")}, "view_read_only"},
		{"loading_until_deadline", []Attempt{loading, backoff}, "repo_loading"},
		{"read_only_without_statement", []Attempt{refused("view_read_only: view is read-only")}, ""},
		{"other_view_building", []Attempt{refused("view_building: checkout route moved before ancestry pin")}, ""},
		{"refusal_past_deadline", []Attempt{late}, ""},
		{"backoff_without_refusal", []Attempt{backoff}, ""},
		{"busy_then_request_outstanding_at_deadline", []Attempt{busy, busy, outstanding}, ""},
		{"no_attempt", nil, ""},
	} {
		if got := editProbeNotAdmitted(tc.attempts); got != tc.want {
			t.Errorf("%s: not admitted %q, want %q", tc.name, got, tc.want)
		}
	}
}
