package main

import (
	"encoding/hex"
	"errors"
	"math"
	"strconv"
	"strings"
)

type editProbeMovePath struct{ Relative, Qualified, Absolute string }
type editProbeMoveFileReference struct{ Path, Role, Receipt string }
type editProbeMoveReferences struct {
	OperationID    string                       `json:"operation_id"`
	SourceID       string                       `json:"source_id"`
	TargetID       string                       `json:"target_id"`
	Files          []editProbeMoveFileReference `json:"files"`
	ReferenceError string                       `json:"reference_error,omitempty"`
}

func movePathMatches(path string, want editProbeMovePath) bool {
	return path != "" && (path == want.Relative || path == want.Qualified || path == want.Absolute)
}
func movePositiveSuffix(value, prefix string) bool {
	if !strings.HasPrefix(value, prefix) {
		return false
	}
	suffix := strings.TrimPrefix(value, prefix)
	n, err := strconv.ParseUint(suffix, 10, 64)
	return err == nil && n > 0 && strconv.FormatUint(n, 10) == suffix
}

// Source acknowledgement and timing references are different facts. Missing
// observations cannot negate a successful correctly identified source move.
func decodeEditProbeMoveAcknowledgement(body map[string]any, sourceID, targetID string, paths map[string]editProbeMovePath) (editProbeMoveReferences, error) {
	out := editProbeMoveReferences{SourceID: sourceID, TargetID: targetID}
	count, countErr := strconvEditProbeUint(body["files_touched"])
	touched, ok := body["touched"].([]any)
	if sourceID == "" || targetID == "" || sourceID == targetID || body["source_id"] != sourceID || body["target_id"] != targetID || body["same_package"] != true || body["dry_run"] != false || countErr != nil || count != 2 || !ok || len(touched) != 2 {
		return out, errors.New("move acknowledgement is not the offered same-package two-file source result")
	}
	seen := map[string]bool{}
	for _, raw := range touched {
		row, ok := raw.(map[string]any)
		if !ok {
			return out, errors.New("move touched row malformed")
		}
		role, _ := row["role"].(string)
		path, _ := row["path"].(string)
		want, known := paths[role]
		if !known || seen[role] || !movePathMatches(path, want) {
			return out, errors.New("move touched identity mismatch")
		}
		seen[role] = true
	}
	if !seen["source"] || !seen["target"] {
		return out, errors.New("move source/target authority incomplete")
	}
	out.OperationID, _ = body["move_observation_id"].(string)
	refs, refsOK := body["file_observations"].([]any)
	if !movePositiveSuffix(out.OperationID, "move-") || !refsOK || len(refs) != 2 || body["mutation_receipt"] != nil {
		out.ReferenceError = "standalone move observation references unavailable"
		return out, nil
	}
	seen = map[string]bool{}
	receipts := map[string]bool{}
	for _, raw := range refs {
		row, ok := raw.(map[string]any)
		if !ok {
			out.ReferenceError = "malformed move file observation"
			break
		}
		role, _ := row["role"].(string)
		path, _ := row["path"].(string)
		receipt, _ := row["receipt"].(string)
		want, known := paths[role]
		if !known || seen[role] || receipts[receipt] || !movePathMatches(path, want) || !movePositiveSuffix(receipt, "move-file-") {
			out.ReferenceError = "foreign/duplicate move observation reference"
			break
		}
		seen[role], receipts[receipt] = true, true
		out.Files = append(out.Files, editProbeMoveFileReference{Path: path, Role: role, Receipt: receipt})
	}
	if len(out.Files) != 2 || !seen["source"] || !seen["target"] {
		out.ReferenceError = "move observation source/target references incomplete"
	}
	return out, nil
}

type editProbeMoveFileAccounting struct {
	Path            string           `json:"path"`
	Role            string           `json:"role"`
	Receipt         string           `json:"receipt"`
	Blob            string           `json:"accepted_blob_sha"`
	OwnerGeneration uint64           `json:"owner_generation"`
	WriteStartedNS  int64            `json:"write_started_ns"`
	WriteReturnedNS int64            `json:"write_returned_ns"`
	WriteWindowNS   int64            `json:"write_window_ns"`
	D               *editProbeWindow `json:"direct_reconcile,omitempty"`
}
type editProbeMoveAccounting struct {
	State   string                        `json:"state"`
	RPCID   string                        `json:"ack_rpc_id"`
	A       *editProbeWindow              `json:"acknowledgement,omitempty"`
	Files   []editProbeMoveFileAccounting `json:"files"`
	Reasons []string                      `json:"reasons,omitempty"`
}

func moveBoundedNS(value any) (int64, error) {
	n, err := strconvEditProbeUint(value)
	if err != nil || n > math.MaxInt64 {
		return 0, errors.New("missing/invalid monotonic nanoseconds")
	}
	return int64(n), nil
}

// This discriminator is deliberately separate from physical A/P/B receipt
// accounting. No missing direct map can fall back to an unrelated physical P,
// watcher generation, batch transaction or synthetic zero background window.
func checkEditProbeMoveAccounting(statuses map[string]map[string]any, refs editProbeMoveReferences, wireID int64) editProbeMoveAccounting {
	out := editProbeMoveAccounting{State: "unavailable"}
	fail := func(reason string) { out.Reasons = append(out.Reasons, reason) }
	if refs.validateReferences() != nil || wireID <= 0 {
		fail("move references/wire identity unavailable")
		return out
	}
	out.RPCID = "rpc-" + strings.TrimPrefix(refs.OperationID, "move-")
	ackHash, session := "", ""
	for _, ref := range refs.Files {
		row := statuses[ref.Role]
		file := editProbeMoveFileAccounting{Path: ref.Path, Role: ref.Role, Receipt: ref.Receipt}
		if row["found"] != true || row["receipt_kind"] != "move_file_observation" || row["receipt"] != ref.Receipt || row["operation_id"] != refs.OperationID || row["source_id"] != refs.SourceID || row["target_id"] != refs.TargetID || row["path"] != ref.Path || row["role"] != ref.Role {
			fail("move file observation identity mismatch")
			out.Files = append(out.Files, file)
			continue
		}
		if row["move_write_verdict"] != "all_writes_completed" || row["file_write_status"] != "committed" || row["writer_return_observed"] != true {
			fail("move source writes not confirmed")
		}
		if row["requested_background_state"] != "not_applicable_direct_reconcile" || row["requested_background"] != nil {
			fail("move direct route/background applicability mismatch")
		}
		blob, _ := row["new_sha"].(string)
		_, blobErr := hex.DecodeString(blob)
		file.Blob = blob
		if len(blob) != 40 || blobErr != nil {
			fail("move accepted blob unavailable")
		}
		ack, _ := row["acknowledgement"].(map[string]any)
		if ack["rpc_id"] != out.RPCID || ack["clock"] != "monotonic" || ack["scope"] != "shared_rpc_mcp_bound_call" || ack["disjoint_observed_intervals"] != true || ack["parse_scope"] != "explicit_go_parser_calls_formatting_unattributed" {
			fail("shared move acknowledgement identity unavailable")
		}
		hash, hashErr := hashJSON(ack)
		if hashErr != nil || ackHash != "" && hash != ackHash {
			fail("shared move acknowledgement differs across files")
		}
		if ackHash == "" {
			ackHash = hash
		}
		phases, _ := ack["observed_execution_ms"].(map[string]any)
		states, _ := ack["phase_states"].(map[string]any)
		applicability, _ := row["acknowledgement_phase_applicability"].(map[string]any)
		counts, _ := ack["counts"].(map[string]any)
		for _, name := range []string{"caller_result", "disk_write", "edit_control", "middleware", "parse", "publish", "reindex_wait", "reply_prepare"} {
			value, present := phases[name]
			if name == "publish" || name == "reindex_wait" {
				count, _ := counts[name].(map[string]any)
				started, se := strconvEditProbeUint(count["started"])
				completed, ce := strconvEditProbeUint(count["completed"])
				na, ne := strconvEditProbeUint(count["not_applicable"])
				if !present || value != nil || states[name] != "unobserved" || applicability[name] != "not_applicable_direct_reconcile" || se != nil || ce != nil || ne != nil || started != 0 || completed != 0 || na != 0 {
					fail("move scheduler A phase is not explicitly inapplicable: " + name)
				}
			} else if !present || value == nil || states[name] != "measured" {
				fail("missing applicable move A phase " + name)
			}
		}
		a, err := checkEditProbeWindow(ack["caller_response_ready_ms"], phases)
		if err != nil {
			fail("move A: " + err.Error())
		} else if out.A == nil {
			out.A = &a
		}
		timing, _ := row["write_timing"].(map[string]any)
		start, se := moveBoundedNS(timing["started_ns"])
		returned, re := moveBoundedNS(timing["returned_ns"])
		wall, we := moveBoundedNS(timing["window_ns"])
		if timing["scope"] != "move_file_write" || timing["clock"] != "monotonic" || timing["state"] != "measured" || timing["outcome"] != "committed" || se != nil || re != nil || we != nil || returned < start || wall != returned-start {
			fail("move file write window unavailable")
		}
		file.WriteStartedNS, file.WriteReturnedNS, file.WriteWindowNS = start, returned, wall
		direct, _ := row["direct_reconcile"].(map[string]any)
		owner, oe := strconvEditProbeUint(direct["owner_generation"])
		dwall, de := moveBoundedNS(direct["window_ns"])
		toAcceptance, ae := moveBoundedNS(direct["writer_return_to_acceptance_ns"])
		if row["direct_reconcile_state"] != "observed" || direct["scope"] != "move_direct_reconcile" || direct["clock"] != "monotonic" || direct["accepted_blob_sha"] != blob || direct["additive_with_acknowledgement"] != false || oe != nil || owner == 0 || de != nil || dwall <= 0 || ae != nil {
			fail("move direct accepted owner/window unavailable")
		}
		if out.A != nil && (returned > out.A.WallNS || toAcceptance > out.A.WallNS-returned || dwall > out.A.WallNS) {
			fail("move publication/write exceeds its actual acknowledgement bounds")
		}
		file.OwnerGeneration = owner
		causes, _ := direct["reconcile_causes"].([]any)
		matchedCause := false
		seen := map[uint64]bool{}
		if len(causes) == 0 || len(causes) > 8 {
			fail("move original reconcile causes unavailable")
		}
		for _, raw := range causes {
			cause, _ := raw.(map[string]any)
			generation, ge := strconvEditProbeUint(cause["admission_generation"])
			s, _ := cause["session"].(string)
			key, _ := cause["request_id_key"].(string)
			if ge != nil || generation == 0 || generation > owner || seen[generation] || s == "" || key == "" {
				fail("invalid move reconcile cause")
			}
			seen[generation] = true
			if key == "n:"+strconv.FormatInt(wireID, 10) {
				if session == "" {
					session = s
				}
				if s != session {
					fail("move per-file causes came from different sessions")
				}
				matchedCause = true
			}
		}
		if !matchedCause {
			fail("move original wire cause not retained")
		}
		accounting, _ := direct["phase_accounting"].(map[string]any)
		phaseOwner, poe := strconvEditProbeUint(accounting["owner_generation"])
		phaseWall, pwe := moveBoundedNS(accounting["window_ns"])
		dphases, _ := accounting["phases_ms"].(map[string]any)
		dstates, _ := accounting["phase_states"].(map[string]any)
		if accounting["scope"] != "move_direct_reconcile" || accounting["clock"] != "monotonic" || accounting["additive_with_requested_background"] != false || accounting["observed_partition_complete"] != true || accounting["sum_state"] != "partial" || poe != nil || phaseOwner != owner || pwe != nil || phaseWall != dwall || len(dphases) != 9 || len(dstates) != 9 {
			fail("move own direct phase binding unavailable")
		}
		for _, name := range []string{"gate_wait", "patch_parse", "apply", "search_lap", "resolve", "mtimes", "derived", "patch_analysis", "repair_control"} {
			if dphases[name] == nil || dstates[name] != "measured" {
				fail("missing applicable move direct phase " + name)
			}
		}
		d, err := checkEditProbeWindowNS(dwall, dphases)
		subtotal, sue := moveBoundedNS(accounting["observed_subtotal_ns"])
		residual, rse := moveBoundedNS(accounting["unattributed_ns"])
		if err != nil || d.ResidualNS != 0 || sue != nil || subtotal != d.SumNS || rse != nil || residual != 0 {
			fail("move direct partition disagrees with actual window")
		} else {
			file.D = &d
		}
		out.Files = append(out.Files, file)
	}
	if len(out.Reasons) == 0 {
		out.State = "observed_move_windows_checks_pass"
	}
	return out
}

func (r editProbeMoveReferences) validateReferences() error {
	if r.ReferenceError != "" {
		return errors.New(r.ReferenceError)
	}
	if !movePositiveSuffix(r.OperationID, "move-") || len(r.Files) != 2 || r.SourceID == "" || r.TargetID == "" || r.SourceID == r.TargetID {
		return errors.New("move references unavailable")
	}
	roles, receipts, paths := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, file := range r.Files {
		if (file.Role != "source" && file.Role != "target") || roles[file.Role] || file.Path == "" || paths[file.Path] || !movePositiveSuffix(file.Receipt, "move-file-") || receipts[file.Receipt] {
			return errors.New("move file references are foreign or repeated")
		}
		roles[file.Role], receipts[file.Receipt], paths[file.Path] = true, true, true
	}
	return nil
}
