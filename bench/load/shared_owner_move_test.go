package main

import (
	"encoding/json"
	"testing"
)

// The actual native transport receipt records both move preparation and the
// committed-file projection's parser calls inside the same real ACK window.
func TestMoveQueuedConsumerAcceptsActualMixedParserScope(t *testing.T) {
	fixture := func() (editProbeMoveReferences, map[string]map[string]any) {
		refs, rows := moveQueuedFixture(t, "admitted_fast")
		for _, row := range rows {
			ack := row["acknowledgement"].(map[string]any)
			ack["parse_scope"] = "mixed_observed_parser_paths"
			ack["counts"].(map[string]any)["parse"] = map[string]any{"started": json.Number("4"), "completed": json.Number("4"), "not_applicable": json.Number("0")}
		}
		return refs, rows
	}
	refs, rows := fixture()
	got := checkEditProbeMoveAccounting(rows, refs, 17)
	if got.State != "qualified_queued_ack_write_only" || got.A == nil {
		t.Fatalf("current real parser scope rejected actual shared ACK/write evidence: %+v", got)
	}
	for _, fault := range []string{"unknown_scope", "missing_counts", "unfinished_parse", "zero_calls", "inapplicable_parse"} {
		t.Run(fault, func(t *testing.T) {
			_, copy := fixture()
			for _, row := range copy {
				ack := row["acknowledgement"].(map[string]any)
				counts := ack["counts"].(map[string]any)["parse"].(map[string]any)
				switch fault {
				case "unknown_scope":
					ack["parse_scope"] = "unspecified_mixed_work"
				case "missing_counts":
					delete(ack["counts"].(map[string]any), "parse")
				case "unfinished_parse":
					counts["completed"] = json.Number("3")
				case "zero_calls":
					counts["started"], counts["completed"] = json.Number("0"), json.Number("0")
				case "inapplicable_parse":
					counts["not_applicable"] = json.Number("1")
				}
			}
			got := checkEditProbeMoveAccounting(copy, refs, 17)
			if got.State == "qualified_queued_ack_write_only" || got.State == "observed_move_windows_checks_pass" {
				t.Fatalf("unknown or incomplete parser observation received ACK credit: %s %+v", fault, got)
			}
		})
	}
}
