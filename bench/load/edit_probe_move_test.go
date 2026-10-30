package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func moveAckFixture() (map[string]any, map[string]editProbeMovePath) {
	paths := map[string]editProbeMovePath{"source": {"source.go", "fixture/source.go", "/owned/source.go"}, "target": {"target.go", "fixture/target.go", "/owned/target.go"}}
	body := map[string]any{"source_id": "fixture/source.go::Relocated", "target_id": "fixture/target.go::Relocated", "same_package": true, "dry_run": false, "files_touched": json.Number("2"), "touched": []any{map[string]any{"path": "fixture/source.go", "role": "source"}, map[string]any{"path": "fixture/target.go", "role": "target"}}, "move_observation_id": "move-31", "file_observations": []any{map[string]any{"path": "fixture/source.go", "role": "source", "receipt": "move-file-52"}, map[string]any{"path": "fixture/target.go", "role": "target", "receipt": "move-file-53"}}}
	return body, paths
}
func moveAccountingFixture(t *testing.T) (editProbeMoveReferences, map[string]map[string]any) {
	t.Helper()
	body, paths := moveAckFixture()
	refs, err := decodeEditProbeMoveAcknowledgement(body, "fixture/source.go::Relocated", "fixture/target.go::Relocated", paths)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]map[string]any{}
	for i, ref := range refs.Files {
		phases, states, counts := map[string]any{}, map[string]any{}, map[string]any{}
		for _, name := range []string{"caller_result", "disk_write", "edit_control", "middleware", "parse", "publish", "reindex_wait", "reply_prepare"} {
			phases[name], states[name] = json.Number("10"), "measured"
			counts[name] = map[string]any{"started": json.Number("1"), "completed": json.Number("1"), "not_applicable": json.Number("0")}
		}
		for _, name := range []string{"publish", "reindex_wait"} {
			phases[name], states[name] = nil, "unobserved"
			counts[name] = map[string]any{"started": json.Number("0"), "completed": json.Number("0"), "not_applicable": json.Number("0")}
		}
		ack := map[string]any{"rpc_id": "rpc-31", "scope": "shared_rpc_mcp_bound_call", "clock": "monotonic", "parse_scope": "explicit_go_parser_calls_formatting_unattributed", "disjoint_observed_intervals": true, "caller_response_ready_ms": json.Number("60"), "observed_execution_ms": phases, "phase_states": states, "counts": counts}
		owner := json.Number("42")
		blob := strings.Repeat("a", 40)
		if i == 1 {
			owner = json.Number("43")
			blob = strings.Repeat("b", 40)
		}
		dp, ds := map[string]any{}, map[string]any{}
		for _, name := range []string{"gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived", "patch_analysis", "repair_control"} {
			dp[name], ds[name] = json.Number("1"), "measured"
		}
		direct := map[string]any{"scope": "move_direct_reconcile", "clock": "monotonic", "owner_generation": owner, "window_ns": json.Number("9000000"), "writer_return_to_acceptance_ns": json.Number("2000000"), "accepted_blob_sha": blob, "additive_with_acknowledgement": false, "reconcile_causes": []any{map[string]any{"session": "actual-session", "request_id_key": "n:17", "admission_generation": json.Number("7")}}, "phase_accounting": map[string]any{"scope": "move_direct_reconcile", "clock": "monotonic", "owner_generation": owner, "window_ns": json.Number("9000000"), "observed_subtotal_ns": json.Number("9000000"), "unattributed_ns": json.Number("0"), "sum_state": "partial", "observed_partition_complete": true, "additive_with_requested_background": false, "phases_ms": dp, "phase_states": ds}}
  direct["request_cause"] = map[string]any{"session": "actual-session", "request_id_key": "n:17", "admission_generation": json.Number("7")}
  direct["started_from_ack_ns"], direct["finished_from_ack_ns"], direct["accepted_from_start_ns"] = json.Number("4000000"), json.Number("13000000"), json.Number("1000000")
		statuses[ref.Role] = map[string]any{"found": true, "receipt_kind": "move_file_observation", "receipt": ref.Receipt, "operation_id": refs.OperationID, "source_id": refs.SourceID, "target_id": refs.TargetID, "path": ref.Path, "role": ref.Role, "move_write_verdict": "all_writes_completed", "file_write_status": "committed", "writer_return_observed": true, "new_sha": blob, "requested_background_state": "not_applicable_direct_reconcile", "acknowledgement_phase_applicability": map[string]any{"publish": "not_applicable_direct_reconcile", "reindex_wait": "not_applicable_direct_reconcile"}, "acknowledgement": ack, "write_timing": map[string]any{"scope": "move_file_write", "clock": "monotonic", "state": "measured", "outcome": "committed", "started_ns": json.Number("1000000"), "returned_ns": json.Number("3000000"), "window_ns": json.Number("2000000")}, "direct_reconcile_state": "observed", "direct_reconcile": direct}
  statuses[ref.Role]["move_request_cause"] = map[string]any{"session": "actual-session", "request_id_key": "n:17"}
	}
	return refs, statuses
}

func TestMoveConsumerKeepsSourceAcknowledgementSeparateFromTimingReferences(t *testing.T) {
	for _, variant := range []string{"valid", "missing_refs", "legacy_receipt", "foreign_ref", "duplicate_role", "wrong_source"} {
		t.Run(variant, func(t *testing.T) {
			body, paths := moveAckFixture()
			switch variant {
			case "missing_refs":
				delete(body, "file_observations")
			case "legacy_receipt":
				body["mutation_receipt"] = "physical-foreign"
			case "foreign_ref":
				body["file_observations"].([]any)[0].(map[string]any)["path"] = "other/source.go"
			case "duplicate_role":
				body["file_observations"].([]any)[1].(map[string]any)["role"] = "source"
			case "wrong_source":
				body["source_id"] = "other/source.go::Relocated"
			}
			refs, err := decodeEditProbeMoveAcknowledgement(body, "fixture/source.go::Relocated", "fixture/target.go::Relocated", paths)
			if variant == "wrong_source" {
				if err == nil {
					t.Fatal("foreign source authority accepted")
				}
				return
			}
			if err != nil {
				t.Fatalf("timing loss negated genuine source acknowledgement: %v", err)
			}
			if (refs.validateReferences() == nil) != (variant == "valid") {
				t.Fatalf("reference validation wrong: %+v", refs)
			}
		})
	}
}
func TestMoveConsumerUsesSeparateOwnedAWriteAndDirectWindows(t *testing.T) {
	refs, statuses := moveAccountingFixture(t)
	got := checkEditProbeMoveAccounting(statuses, refs, 17)
	if got.State != "observed_move_windows_checks_pass" || got.A == nil || got.A.WallNS != 60000000 || len(got.Files) != 2 {
		t.Fatalf("owned move accounting refused/doubled shared A: %+v", got)
	}
	if got.Files[0].OwnerGeneration == 31 || got.Files[0].OwnerGeneration == 17 {
		t.Fatal("independent executor owner was replaced by ACK/wire domain")
	}
	for _, file := range got.Files {
		if file.D == nil || file.D.WallNS != 9000000 || file.D.ResidualNS != 0 || (file.WriteWindowNS == nil || *file.WriteWindowNS != 2000000) {
			t.Fatalf("bad own-file windows: %+v", file)
		}
	}
}
func TestMoveConsumerRefusesMissingStandaloneMapsForeignOwnersAndInventedZero(t *testing.T) {
	for _, variant := range []string{"missing_kind", "physical_status", "missing_A", "wrong_A_owner", "missing_scheduler_key", "measured_scheduler_zero", "missing_applicability", "partial_source", "missing_write_map", "wrong_write_wall", "missing_direct_map", "foreign_blob", "foreign_phase_owner", "foreign_wire", "split_session", "missing_deep_map", "missing_deep_phase", "unmeasured_deep_phase", "nonzero_residual", "outside_A", "legacy_maps_only", "duplicate_file_role"} {
		t.Run(variant, func(t *testing.T) {
			refs, statuses := moveAccountingFixture(t)
			row := statuses["source"]
			ack := row["acknowledgement"].(map[string]any)
			direct := row["direct_reconcile"].(map[string]any)
			accounting := direct["phase_accounting"].(map[string]any)
			switch variant {
			case "missing_kind":
				delete(row, "receipt_kind")
			case "physical_status":
				row["receipt_kind"] = "physical_mutation"
			case "missing_A":
				delete(row, "acknowledgement")
			case "wrong_A_owner":
				ack["rpc_id"] = "rpc-foreign"
			case "missing_scheduler_key":
				delete(ack["observed_execution_ms"].(map[string]any), "publish")
			case "measured_scheduler_zero":
				ack["observed_execution_ms"].(map[string]any)["publish"] = json.Number("0")
				ack["phase_states"].(map[string]any)["publish"] = "measured"
			case "missing_applicability":
				delete(row, "acknowledgement_phase_applicability")
			case "partial_source":
				row["move_write_verdict"] = "partial_or_failed"
			case "missing_write_map":
				delete(row, "write_timing")
			case "wrong_write_wall":
				row["write_timing"].(map[string]any)["window_ns"] = json.Number("1")
			case "missing_direct_map":
				delete(row, "direct_reconcile")
			case "foreign_blob":
				direct["accepted_blob_sha"] = strings.Repeat("c", 40)
			case "foreign_phase_owner":
				accounting["owner_generation"] = json.Number("9")
			case "foreign_wire":
				direct["reconcile_causes"].([]any)[0].(map[string]any)["request_id_key"] = "n:18"
			case "split_session":
				statuses["target"]["direct_reconcile"].(map[string]any)["reconcile_causes"].([]any)[0].(map[string]any)["session"] = "foreign-session"
			case "missing_deep_map":
				delete(accounting, "phases_ms")
			case "missing_deep_phase":
				delete(accounting["phases_ms"].(map[string]any), "resolve")
			case "unmeasured_deep_phase":
				accounting["phase_states"].(map[string]any)["resolve"] = "unobserved"
			case "nonzero_residual":
				accounting["unattributed_ns"] = json.Number("1")
			case "outside_A":
				direct["writer_return_to_acceptance_ns"] = json.Number("60000000")
			case "legacy_maps_only":
				delete(row, "direct_reconcile")
				row["phase_timing"] = editProbeRepairAccountingFixture()["phase_timing"]
			case "duplicate_file_role":
				refs.Files[1] = refs.Files[0]
			}
			got := checkEditProbeMoveAccounting(statuses, refs, 17)
			if got.State == "observed_move_windows_checks_pass" || len(got.Reasons) == 0 {
				t.Fatalf("%s falsely supplied move coverage: %+v", variant, got)
			}
		})
	}
}

func moveReadAssistedFixture(row map[string]any) map[string]any {
 repair := row["direct_reconcile"].(map[string]any)
 delete(row, "direct_reconcile")
 row["direct_reconcile_state"] = "unavailable_owner_bytes_or_acceptance"
 row["read_assisted_reconcile_state"], row["read_assisted_reconcile"] = "observed", repair
 repair["scope"] = "move_read_assisted_reconcile"
 repair["phase_accounting"].(map[string]any)["scope"] = "move_read_assisted_reconcile"
 cause := map[string]any{"session": "independent-reader", "request_id_key": "n:23", "admission_generation": json.Number("7")}
 repair["request_cause"], repair["reconcile_causes"] = cause, []any{cause}
 return repair
}

func TestMoveConsumerNamesActualReadAssistedRouteWithoutInventingDirectWork(t *testing.T) {
 refs, statuses := moveAccountingFixture(t)
 repair := moveReadAssistedFixture(statuses["target"])
 // Independent R can start before shared A. Retain its full non-additive90ms
 // window; actual acceptance5ms after A entry still follows writer return3ms.
 repair["started_from_ack_ns"], repair["finished_from_ack_ns"], repair["accepted_from_start_ns"] = json.Number("-40000000"), json.Number("50000000"), json.Number("45000000")
 repair["window_ns"] = json.Number("90000000")
 phase := repair["phase_accounting"].(map[string]any)
 phase["window_ns"], phase["observed_subtotal_ns"] = json.Number("90000000"), json.Number("90000000")
 for name := range phase["phases_ms"].(map[string]any) { phase["phases_ms"].(map[string]any)[name] = json.Number("10") }
 got := checkEditProbeMoveAccounting(statuses, refs, 17)
 if got.State != "observed_move_windows_checks_pass" || len(got.Files) != 2 || got.Files[0].Route != "direct_reconcile" || got.Files[1].Route != "read_assisted_reconcile" || got.Files[1].D != nil || got.Files[1].R == nil || got.Files[1].R.WallNS != 90000000 || got.A == nil || got.A.WallNS != 60000000 { t.Fatalf("borrowed or truncated publication window: %+v", got) }
}

func TestMoveReadAssistedConsumerRejectsForeignCauseAndInvalidActualAcceptance(t *testing.T) {
 for _, name := range []string{"missing_move_cause", "foreign_move_wire", "missing_actual_cause", "ack_cause_as_reader", "foreign_admission", "missing_window_start", "missing_acceptance", "acceptance_outside_window", "acceptance_before_write", "acceptance_disagrees", "wrong_route_scope", "both_routes"} {
  t.Run(name, func(t *testing.T) {
   refs, statuses := moveAccountingFixture(t)
   row := statuses["target"]
   repair := moveReadAssistedFixture(row)
   switch name {
   case "missing_move_cause": delete(row, "move_request_cause")
   case "foreign_move_wire": row["move_request_cause"].(map[string]any)["request_id_key"] = "n:99"
   case "missing_actual_cause": delete(repair, "request_cause")
   case "ack_cause_as_reader": repair["request_cause"].(map[string]any)["session"] = "actual-session";repair["request_cause"].(map[string]any)["request_id_key"] = "n:17"
   case "foreign_admission": repair["request_cause"] = map[string]any{"session": "other-reader", "request_id_key": "n:999", "admission_generation": json.Number("7")}
   case "missing_window_start": delete(repair, "started_from_ack_ns")
   case "missing_acceptance": delete(repair, "accepted_from_start_ns")
   case "acceptance_outside_window": repair["accepted_from_start_ns"] = json.Number("10000000")
   case "acceptance_before_write": repair["started_from_ack_ns"], repair["finished_from_ack_ns"] = json.Number("0"), json.Number("9000000");repair["writer_return_to_acceptance_ns"] = json.Number("0")
   case "acceptance_disagrees": repair["writer_return_to_acceptance_ns"] = json.Number("3")
   case "wrong_route_scope": repair["scope"] = "move_direct_reconcile"
   case "both_routes": row["direct_reconcile_state"],row["direct_reconcile"] = "observed",repair
   }
   if got := checkEditProbeMoveAccounting(statuses, refs, 17); got.State == "observed_move_windows_checks_pass" { t.Fatal("invalid read route accepted", name, got) }
  })
 }
}
