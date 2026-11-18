package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func moveQueuedFixture(t *testing.T, mode string) (editProbeMoveReferences, map[string]map[string]any) {
	t.Helper()
	refs, rows := moveAccountingFixture(t)
	for _, row := range rows {
		delete(row, "direct_reconcile")
		delete(row, "read_assisted_reconcile")
		row["direct_reconcile_state"], row["read_assisted_reconcile_state"] = "unavailable_queued_publication", "unavailable_queued_publication"
		row["requested_background_state"] = "admitted"
		app := map[string]any{"publish": "applicable", "reindex_wait": "applicable"}
		row["acknowledgement_phase_applicability"] = app
		ack := row["acknowledgement"].(map[string]any)
		phases := ack["observed_execution_ms"].(map[string]any)
		states := ack["phase_states"].(map[string]any)
		counts := ack["counts"].(map[string]any)
		for _, name := range []string{"publish", "reindex_wait"} {
			phases[name], states[name] = json.Number("10"), "measured"
			counts[name] = map[string]any{"started": json.Number("1"), "completed": json.Number("1"), "not_applicable": json.Number("0")}
		}
		ack["caller_response_ready_ms"] = json.Number("80")
		if mode != "admitted_wait" {
			ack["caller_response_ready_ms"] = json.Number("70")
			phases["reindex_wait"], states["reindex_wait"] = nil, "unobserved"
			counts["reindex_wait"] = map[string]any{"started": json.Number("0"), "completed": json.Number("0"), "not_applicable": json.Number("0")}
			app["reindex_wait"] = "not_applicable_fast_ack"
		}
		if mode == "admission_failed" {
			row["requested_background_state"] = "admission_failed"
			app["reindex_wait"] = "not_observed"
			counts["reindex_wait"].(map[string]any)["not_applicable"] = json.Number("0")
		}
	}
	return refs, rows
}

func TestMoveQueuedConsumerKeepsACKAndWriteQualifiedWithoutPublicationCredit(t *testing.T) {
	for _, mode := range []string{"admitted_wait", "admitted_fast", "admission_failed"} {
		t.Run(mode, func(t *testing.T) {
			refs, rows := moveQueuedFixture(t, mode)
			got := checkEditProbeMoveAccounting(rows, refs, 17)
			if got.State != "qualified_queued_ack_write_only" || got.A == nil || len(got.Files) != 2 {
				t.Fatalf("known queued route lost its qualified actual ACK/write observations: %+v", got)
			}
			if len(got.Reasons) == 0 {
				t.Fatal("queued publication must retain the missing physical P/B and direct/R qualification")
			}
			for _, file := range got.Files {
				if file.D != nil || file.R != nil || file.WriteWindowNS == nil || !strings.HasPrefix(file.Route, "queued_publication") {
					t.Fatalf("queued row borrowed or invented a publication window: %+v", file)
				}
			}
		})
	}
}

func TestMoveQueuedConsumerRefusesContradictionsAndForeignACKOrCause(t *testing.T) {
	for _, fault := range []string{"direct_map", "background_map", "wire_cause", "foreign_ack", "write_window", "publish_phase", "missing_route", "wait_count", "invented_fast_wait", "outside_ack", "split_session"} {
		t.Run(fault, func(t *testing.T) {
			refs, rows := moveQueuedFixture(t, "admitted_wait")
			row := rows["source"]
			switch fault {
			case "direct_map":
				row["direct_reconcile"] = map[string]any{"window_ns": json.Number("0")}
			case "background_map":
				row["requested_background"] = map[string]any{"background_wall_ms": json.Number("0")}
			case "wire_cause":
				row["move_request_cause"].(map[string]any)["request_id_key"] = "n:18"
			case "foreign_ack":
				row["acknowledgement"].(map[string]any)["rpc_id"] = "foreign"
			case "write_window":
				delete(row, "write_timing")
			case "publish_phase":
				row["acknowledgement"].(map[string]any)["observed_execution_ms"].(map[string]any)["publish"] = nil
			case "wait_count":
				row["acknowledgement"].(map[string]any)["counts"].(map[string]any)["reindex_wait"].(map[string]any)["completed"] = json.Number("0")
			case "invented_fast_wait":
				for _, r := range rows {
					r["acknowledgement_phase_applicability"].(map[string]any)["reindex_wait"] = "not_applicable_fast_ack"
				}
			case "outside_ack":
				row["write_timing"].(map[string]any)["returned_ns"] = json.Number("100000000")
				row["write_timing"].(map[string]any)["window_ns"] = json.Number("99000000")
			case "split_session":
				row["move_request_cause"].(map[string]any)["session"] = "foreign-session"
			case "missing_route":
				delete(row, "requested_background_state")
			}
			got := checkEditProbeMoveAccounting(rows, refs, 17)
			if got.State == "qualified_queued_ack_write_only" || got.State == "observed_move_windows_checks_pass" {
				t.Fatalf("invalid queued evidence credited: %s %+v", fault, got)
			}
		})
	}
}

func TestMoveQueuedConsumerSharesRealACKWaitWithoutAssigningItToFailedSibling(t *testing.T) {
	refs, rows := moveQueuedFixture(t, "admitted_wait")
	rows["target"]["requested_background_state"] = "admission_failed"
	rows["target"]["acknowledgement_phase_applicability"].(map[string]any)["reindex_wait"] = "not_observed"
	got := checkEditProbeMoveAccounting(rows, refs, 17)
	if got.State != "qualified_queued_ack_write_only" || got.A == nil {
		t.Fatalf("shared real ACK wait lost or became per-file publication: %+v", got)
	}
	for _, file := range got.Files {
		if file.D != nil || file.R != nil {
			t.Fatal("shared ACK wait borrowed as publication")
		}
	}
}

func TestMoveQueuedConsumerCollectorNeverMarksGraphTerminalFromQualifiedACK(t *testing.T) {
	refs, rows := moveQueuedFixture(t, "admitted_wait")
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		args, _ := frame.Params["arguments"].(map[string]any)
		options, _ := args["options"].(map[string]any)
		for _, file := range refs.Files {
			if options["receipt"] == file.Receipt {
				return stubContent(rows[file.Role]), nil
			}
		}
		return stubContent(map[string]any{"found": false}), nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	client, err := openMCP(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sink, err := os.CreateTemp(t.TempDir(), "queued-move-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	state := &editProbeState{client: client, sink: sink, record: completeMovePublicRecord()}
	state.startRaw()
	defer state.finishRaw()
	run := &editProbeRuntime{options: runOptions{Deadline: time.Second}, p: &privateRun{Repos: []privateRepo{{Prefix: "fixture"}}}, phase: &Phase{Metrics: map[string]Metric{}}, origin: time.Now(), requireAccounting: true}
	run.collectMoveEvidence(ctx, state, refs, 17)
	if ctx.Err() != nil || state.record.MoveAccounting == nil || state.record.MoveAccounting.State != "qualified_queued_ack_write_only" || state.record.GraphTerminal || state.record.Accounting.P != nil || state.record.Accounting.B != nil {
		t.Fatalf("qualified shared ACK became physical publication evidence: %+v %v", state.record, ctx.Err())
	}
}
