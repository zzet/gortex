package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// linkedReceiptJSON is a published linked-checkout edit receipt, trimmed to
// what the accounting check reads. fixed selects the shape a linked receipt
// carries with its own B disposition, P block, counted reindex_wait and
// ticket-domain applied_generation; otherwise it is the earlier shape, which
// named the routed dirty generation as applied and carried no B.
func linkedReceiptJSON(fixed bool) string {
	applied, reindexWait, waitCount, background, patch := "4", `"unobserved"`, `{"started":0,"completed":0,"not_applicable":0}`, "", ""
	if fixed {
		applied, reindexWait, waitCount = "1", `"not_applicable"`, `{"started":1,"completed":1,"not_applicable":1}`
		background = `,"requested_background_unavailable":{"publication_key":"mutation-2","scope":"requested_background_observation",
			"reason":"serving_attempt_binding_mismatch","requested_generation":1,"last_observed_generation":1,"observed_native_handoffs":0,
			"binding_predicate":"checkout_refresh_cycle","background_stage":"returned","accepted_attempt_stage":"physical_window_retained"},
			"dirty_generation_id":4`
		patch = `,"patch_accounting":{"execution_disposition":"checkout_refresh_cycle","attempt_ticket":1,"attempt_retry":0,
			"scope":"accepted_winning_physical_attempt","background_coverage":"not_established","sum_state":"not_observed",
			"observed_partition_complete":false,"phases_ms":{"gate_wait":null,"patch_parse":null,"apply":null},
			"phase_states":{"gate_wait":"not_observed","patch_parse":"not_observed","apply":"not_observed"},
			"observed_subtotal_ms":null,"unattributed_ms":null}`
	}
	patchWall := "null"
	if fixed {
		patchWall = "11618.2"
	}
	return `{"found":true,"receipt":"commit-2","tool":"edit_file","mutation_id":"load-edit-1","checkout_id":"checkout-1",
		"graph_status":"fresh","graph_status_terminal":true,"reindex_receipt":"mutation-2","reindex_generation":1,"applied_generation":` + applied + `,
		"acknowledgement":{"state":"observed","rpc_id":"rpc-2","disjoint_observed_intervals":true,"caller_response_ready_ms":190.5,
			"phases_ms":{"caller_result":0.5,"disk_write":0.5,"edit_control":181.5,"middleware":7,"parse":0.5,"publish":0.5,"reindex_wait":null,"reply_prepare":0},
			"observed_execution_ms":{"caller_result":0.5,"disk_write":0.5,"edit_control":181.5,"middleware":7,"parse":0.5,"publish":0.5,"reindex_wait":null,"reply_prepare":0},
			"phase_states":{"caller_result":"measured","disk_write":"measured","edit_control":"measured","middleware":"measured","parse":"measured","publish":"measured","reindex_wait":` + reindexWait + `,"reply_prepare":"measured"},
			"counts":{"reindex_wait":` + waitCount + `}},
		"phase_timing":{"clock":"monotonic","coverage":"partial","ack_wall_ms":190.5,"patch_wall_ms":` + patchWall + patch + `,"publication_wall_ms":11837.7},
		"publication_phases":{"key":"mutation-2","checkout_id":"checkout-1","source":"mcp_edit","ticket":1,"dirty_generation_id":4,"clock":"monotonic","terminal":true}` +
		background + `}`
}

func decodeLinkedReceipt(t *testing.T, fixed bool) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(strings.NewReader(linkedReceiptJSON(fixed)))
	decoder.UseNumber()
	var status map[string]any
	if err := decoder.Decode(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

// A linked receipt is terminal on the first poll that reads its publication,
// as the primary's is. Its typed B refusal in the ticket's domain is what
// stops collection; without it, and with applied_generation naming the
// routed dirty generation, the record could only wait for its deadline.
func TestEditProbeLinkedCheckoutReceiptAccountingIsTerminalOnFirstPoll(t *testing.T) {
	got := checkEditProbeAccounting(decodeLinkedReceipt(t, true), "commit-2")
	if got.State != "terminal_unavailable_background" || got.Requested != 1 || got.Applied != 1 || got.B != nil || got.P != nil {
		t.Fatalf("linked receipt accounting = %+v, want terminal unavailable background in the ticket's domain", got)
	}
	if !editProbeFinished(true, true, true, got) {
		t.Fatalf("a published linked receipt did not finish collection: %+v", got)
	}
	if editProbeFinished(false, true, true, got) || editProbeFinished(true, false, true, got) {
		t.Fatal("linked accounting bypassed graph/oracle completion")
	}

	before := checkEditProbeAccounting(decodeLinkedReceipt(t, false), "commit-2")
	if before.State != "unavailable" || editProbeFinished(true, true, true, before) {
		t.Fatalf("a receipt without its B disposition stopped collection: %+v", before)
	}
	if !strings.Contains(strings.Join(before.Reasons, "; "), "independent serving identity mismatch") {
		t.Fatalf("a dirty-generation applied_generation should read as a coalesced receipt without a winner: %+v", before.Reasons)
	}
}
