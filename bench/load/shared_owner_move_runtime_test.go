package main

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"
)

// Typed Server-witness models over a real owned child. The native transport
// test separately proves that the real producer supplies these wire objects.
func sharedMoveRuntimeModel(t *testing.T) (*editProbeRuntime, editProbeMoveReferences, map[string]map[string]any) {
	t.Helper()
	p, _, original := sharedOwnerProofModel(t)
	refs, rows := moveQueuedFixture(t, "admitted_fast")
	refs.SourceID, refs.TargetID = "primary/a.go::Moved", "primary/b.go::Moved"
	for i := range refs.Files {
		file := &refs.Files[i]
		name, memberIndex := "a.go", 0
		if file.Role == "target" {
			name, memberIndex = "b.go", 1
		}
		file.Path = "primary/" + name
		row := rows[file.Role]
		row["path"], row["source_id"], row["target_id"] = file.Path, refs.SourceID, refs.TargetID
		child := cloneSharedOwnerModel(t, original)
		proof := child["shared_owner_accounting_v1"].(map[string]any)
		definition := proof["definition"].(map[string]any)
		member := definition["members"].([]any)[memberIndex].(map[string]any)
		proof["reference"].(map[string]any)["member"] = member
		witness := proof["membership"].(map[string]any)
		witness["member"], witness["applied_generation"] = member, member["ticket"]
		child["receipt"], child["path"] = member["receipt"], name
		child["reindex_generation"], child["applied_generation"] = member["ticket"], member["ticket"]
		child["commit_seq"], child["content_sha256"], child["new_sha"] = member["physical_commit_sequence"], member["content_sha256"], row["new_sha"]
		publication := child["publication_phases"].(map[string]any)
		publication["key"], publication["ticket"] = member["publication_key"], member["ticket"]
		ack := row["acknowledgement"].(map[string]any)
		ack["parse_scope"] = "mixed_observed_parser_paths"
		ack["counts"].(map[string]any)["parse"] = map[string]any{"started": json.Number("4"), "completed": json.Number("4"), "not_applicable": json.Number("0")}
		child["acknowledgement"] = ack
		row["physical_commit"], row["physical_commit_state"] = child, "observed"
		row["physical_commit_shared_acknowledgement"], row["physical_commit_additive_with_acknowledgement"] = true, false
	}
	r := &editProbeRuntime{p: p, phase: &Phase{EditProbe: &editProbeReport{Role: "candidate"}}}
	return r, refs, rows
}

func TestSharedMoveRuntimeKeepsOneACKAndDistinctPhysicalReferences(t *testing.T) {
	r, refs, rows := sharedMoveRuntimeModel(t)
	got := r.accountingForMoveStatuses(t.Context(), rows, refs, 17)
	if got.State != "observed_move_shared_owner_windows_checks_pass" || got.A == nil || got.A.WallNS != 70_000_000 || len(got.Files) != 2 {
		t.Fatalf("two current physical members did not keep one independently validated move ACK: %+v", got)
	}
	for _, file := range got.Files {
		if file.D != nil || file.R != nil || file.WriteWindowNS == nil || file.Shared == nil {
			t.Fatalf("shared execution borrowed a per-file D/R or lost actual source write: %+v", file)
		}
	}
	report := r.finishSharedOwners()
	if report == nil || report.DistinctOwners != 1 || report.Execution.Count != 1 || report.Background.Count != 1 {
		t.Fatal("two move members multiplied the one shared E/B owner")
	}
}

func TestSharedMoveCollectorWaitsForActualChildOwner(t *testing.T) {
	r, refs, rows := sharedMoveRuntimeModel(t)
	seen := make(map[string]int)
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		args, _ := frame.Params["arguments"].(map[string]any)
		options, _ := args["options"].(map[string]any)
		for _, file := range refs.Files {
			if options["receipt"] == file.Receipt {
				seen[file.Role]++
				if seen[file.Role] == 1 {
					pending := cloneSharedOwnerModel(t, rows[file.Role])
					child := pending["physical_commit"].(map[string]any)
					delete(child, "shared_owner_accounting_v1")
					child["shared_owner_accounting_v1_availability"] = map[string]any{"state": "pending", "reason": "executor_not_finalized"}
					return stubContent(pending), nil
				}
				return stubContent(rows[file.Role]), nil
			}
		}
		return stubContent(map[string]any{"found": false}), nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	client, err := openMCP(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sink, err := os.CreateTemp(t.TempDir(), "shared-move-model-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	state := &editProbeState{client: client, sink: sink, record: completeMovePublicRecord()}
	state.startRaw()
	defer state.finishRaw()
	r.origin, r.options.Deadline, r.requireAccounting = time.Now(), time.Second, true
	r.collectMoveEvidence(ctx, state, refs, 17)
	if ctx.Err() != nil || !state.record.GraphTerminal || state.record.MoveAccounting == nil || state.record.MoveAccounting.State != "observed_move_shared_owner_windows_checks_pass" {
		t.Fatalf("collector discarded actual child ownership as legacy queued unavailability: %+v %v", state.record.MoveAccounting, ctx.Err())
	}
	if seen["source"] != 2 || seen["target"] != 2 {
		t.Fatalf("pending actual owner was discarded or retried beyond its frozen result: %+v", seen)
	}
}

func TestSharedMoveRuntimeNeverBorrowsMissingOrForeignChild(t *testing.T) {
	for _, fault := range []string{"missing_child", "foreign_ack", "foreign_path", "foreign_blob", "additive_child", "missing_membership", "wrong_physical_sequence", "missing_shared_A", "parent_truncated", "parent_refused", "parent_error"} {
		t.Run(fault, func(t *testing.T) {
			r, refs, rows := sharedMoveRuntimeModel(t)
			row := rows["source"]
			child := row["physical_commit"].(map[string]any)
			switch fault {
			case "missing_child":
				delete(row, "physical_commit")
			case "foreign_ack":
				child["acknowledgement"] = map[string]any{"rpc_id": "foreign"}
			case "foreign_path":
				child["path"] = "b.go"
			case "foreign_blob":
				child["new_sha"] = "foreign"
			case "additive_child":
				row["physical_commit_additive_with_acknowledgement"] = true
			case "missing_membership":
				delete(child["shared_owner_accounting_v1"].(map[string]any), "membership")
			case "wrong_physical_sequence":
				child["commit_seq"] = json.Number("99")
			case "missing_shared_A":
				delete(row, "acknowledgement")
			case "parent_truncated":
				row["truncated"] = true
			case "parent_refused":
				row["refused"] = true
			case "parent_error":
				row["error"] = "incomplete parent result"
			}
			if got := r.accountingForMoveStatuses(t.Context(), rows, refs, 17); got.State == "observed_move_shared_owner_windows_checks_pass" || got.State == "observed_move_windows_checks_pass" {
				t.Fatalf("foreign/missing child received move accounting credit: %s %+v", fault, got)
			}
		})
	}
}

func TestSharedMoveRuntimeLeavesHistoricalDirectRowsUnchanged(t *testing.T) {
	refs, rows := moveAccountingFixture(t)
	r := &editProbeRuntime{}
	if got := r.accountingForMoveStatuses(t.Context(), rows, refs, 17); got.State != "observed_move_windows_checks_pass" || got.A == nil {
		t.Fatalf("historical direct-only evidence acquired an unrelated daemon binding: %+v", got)
	}
}

func TestSharedMoveFinalContradictionRetainsACKAndOfferedMembers(t *testing.T) {
	r, refs, rows := sharedMoveRuntimeModel(t)
	accounting := r.accountingForMoveStatuses(t.Context(), rows, refs, 17)
	if accounting.State != "observed_move_shared_owner_windows_checks_pass" {
		t.Fatal("fixture did not validate its actual owned child/model membership")
	}
	ack := accounting.A
	report := r.finishSharedOwners()
	// The report contradiction detector has its own raw-definition controls;
	// this checks final-record propagation after that permanent state is set.
	entry := report.Definitions[accounting.Files[0].Shared.DefinitionKey]
	entry.State, entry.Execution, entry.Background = "contradictory_definition", nil, nil
	record := editProbeRecord{Ordinal: 7, Outcome: "complete", GraphTerminal: true, Ack: measured(2, "ms"), Move: &refs, MoveAccounting: &accounting}
	invalidateSharedAccounting(&record, report)
	if record.Ordinal != 7 || record.Outcome != "diagnostic_failure" || record.MoveAccounting.A != ack || record.MoveAccounting.State != "unavailable_move_shared_owner" || len(record.MoveAccounting.Files) != 2 || len(record.Errors) == 0 || record.Ack.Value == nil || *record.Ack.Value != 2 || !record.GraphTerminal {
		t.Fatalf("late owner contradiction dropped offered members/ACK facts or kept accounting credit: %+v", record)
	}
	for _, file := range record.MoveAccounting.Files {
		if file.Shared == nil || file.WriteWindowNS == nil || file.D != nil || file.R != nil {
			t.Fatal("late owner failure erased original references/write facts or borrowed a per-file window")
		}
	}
}
