package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zzet/gortex/bench/receiptproof"
)

func TestEditProbeSupersededGuardJoinRequiresOneOwnedObservation(t *testing.T) {
	for _, mode := range []string{"exact", "tee_copy", "missing", "foreign_key", "foreign_repo", "conflicting_guard", "repeated_same_source"} {
		t.Run(mode, func(t *testing.T) {
			_, proof := supersededRecordFixture(t)
			guard := proof.Guard
			guard.SourceLog, guard.Line, guard.SHA256 = "", 0, ""
			if mode == "foreign_key" {
				guard.Key = "foreign"
			}
			if mode == "foreign_repo" {
				guard.Repo = "foreign"
			}
			data, err := json.Marshal(guard)
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			line := append(append([]byte(nil), data...), '\n')
			if mode == "repeated_same_source" {
				line = append(line, line...)
			}
			if mode == "conflicting_guard" {
				guard.Pending = 5
				other, marshalErr := json.Marshal(guard)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				line = append(append(line, other...), '\n')
			}
			if mode != "missing" {
				if err := os.WriteFile(filepath.Join(root, "daemon-1.stderr.log"), line, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "tee_copy" {
				if err := os.WriteFile(filepath.Join(root, "daemon.log"), line, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			got, err := collectSupersededGuards(root, "repo", map[string]uint64{"mutation-a": 3})
			if err != nil {
				t.Fatal(err)
			}
			want := mode == "exact" || mode == "tee_copy"
			if (got["mutation-a"] != nil) != want {
				t.Fatalf("guard join=%+v for %s", got, mode)
			}
			if want && (got["mutation-a"].SHA256 != fmt.Sprintf("%x", sha256.Sum256(data)) || got["mutation-a"].Line != 1) {
				t.Fatal("guard proof lost its exact source reference")
			}
		})
	}
}

func supersededRecordFixture(t *testing.T) (editProbeRecord, *receiptproof.Superseded) {
	t.Helper()
	data, err := os.ReadFile("../receiptproof/testdata/superseded.json")
	if err != nil {
		t.Fatal(err)
	}
	var proof receiptproof.Superseded
	if err := json.Unmarshal(data, &proof); err != nil {
		t.Fatal(err)
	}
	exact := false
	record := editProbeRecord{Ordinal: 1, Editor: 2, Outcome: "diagnostic_failure", Receipt: "commit-a", MutationID: "edit-a", ACKMutationID: "edit-a", ACKStatus: "applied", ACKDiskStatus: "committed", ACKCommitSeq: 2, ACKContentSHA256: strings.Repeat("d", 64), ACKReadbackExact: &exact, GraphTerminal: true, Ack: measured(45, "ms"), FirstCorrect: map[string]Metric{"source": measured(1, "ms")}, EvidenceBytes: 98839, EvidenceLimit: editProbeRawLimit}
	record.Accounting = editProbeAccounting{State: "terminal_unavailable_background", RPCID: "rpc-2", Requested: 3, Applied: 3, A: &editProbeWindow{WallNS: 8, SumNS: 8, WithinFivePercent: true}, Reasons: strings.Split(strings.TrimPrefix(proof.AccountingError, "A/P/B accounting unavailable: "), "; ")}
	for _, name := range []string{"source", "summary", "editing_context", "callers", "call_chain", "search_visible", "text"} {
		record.FirstCorrect[name] = measured(1, "ms")
	}
	for _, name := range []string{"source", "file", "summary", "search_visible", "text"} {
		record.Readbacks = append(record.Readbacks, editProbeReadback{Kind: name, Correct: true, Latency: measured(1, "ms")})
	}
	record.Errors = []string{proof.AccountingError}
	return record, &proof
}

func TestEditProbeProvenSupersededPreservesUnavailableTimingsAndRefusal(t *testing.T) {
	record, proof := supersededRecordFixture(t)
	scope := &editProbePhysicalWriteScope{Kind: "shared_worktree_file", File: "sample.go", Editors: 4}
	if !classifySupersededRecord(&record, scope, proof) {
		t.Fatal("exact proven shared receipt was not classified")
	}
	if record.Outcome != receiptproof.SupersededOutcome || len(record.Errors) != 0 {
		t.Fatalf("classification=%+v", record)
	}
	if record.Accounting.P != nil || record.Accounting.B != nil || record.Accounting.State != "terminal_unavailable_background" || len(record.Accounting.Reasons) == 0 {
		t.Fatal("classification invented or erased indexing evidence")
	}
}

func TestEditProbeSupersededNeverMasksOtherFailures(t *testing.T) {
	for _, mode := range []string{"unknown_scope", "single_writer", "wrong_ACK", "stale", "incorrect_read", "missing_first_correct", "missing_immediate_file", "missing_call_chain", "read_error", "capped", "deadline", "accepted_timing", "foreign_guard"} {
		t.Run(mode, func(t *testing.T) {
			record, proof := supersededRecordFixture(t)
			scope := &editProbePhysicalWriteScope{Kind: "shared_worktree_file", File: "sample.go", Editors: 4}
			switch mode {
			case "unknown_scope":
				scope = nil
			case "single_writer":
				scope.Editors = 1
			case "wrong_ACK":
				record.ACKMutationID = "foreign"
			case "stale":
				record.StaleClaimedFresh = true
			case "incorrect_read":
				record.Readbacks[0].Correct = false
			case "missing_first_correct":
				record.FirstCorrect["source"] = unavailable("ms", "missing observation")
			case "missing_immediate_file":
				record.Readbacks = append(record.Readbacks[:1], record.Readbacks[2:]...)
			case "missing_call_chain":
				delete(record.FirstCorrect, "call_chain")
			case "read_error":
				record.Readbacks[0].Error = "readback failed"
			case "capped":
				record.Outcome = "harness_evidence_incomplete"
			case "deadline":
				record.Outcome = "product_deadline"
			case "accepted_timing":
				record.Accounting.P = &editProbeWindow{WallNS: 1}
			case "foreign_guard":
				proof.Guard.Key = "foreign"
			}
			if classifySupersededRecord(&record, scope, proof) {
				t.Fatal("unproven or failed record classified")
			}
			if record.Accounting.P == nil && record.Outcome == receiptproof.SupersededOutcome {
				t.Fatal("failed record outcome changed")
			}
		})
	}
}

func supersededStatusFixture() map[string]any {
	status := editProbeAccountingFixture()
	status["receipt"], status["mutation_id"] = "commit-a", "edit-a"
	status["reindex_generation"], status["applied_generation"] = json.Number("3"), json.Number("3")
	status["graph_status"], status["graph_status_terminal"] = "fresh", true
	status["publication_phases"] = map[string]any{"key": "mutation-a", "ticket": json.Number("3"), "source": "mcp_edit", "clock": "monotonic", "terminal": true}
	status["phase_timing"] = map[string]any{"patch_wall_ms": nil}
	delete(status, "requested_background")
	delete(status, "coalesced_publication")
	delete(status, "read_assisted_publication")
	status["acknowledgement"].(map[string]any)["rpc_id"] = "rpc-2"
	status["requested_background_unavailable"] = map[string]any{"publication_key": "mutation-a", "scope": "requested_background_observation", "reason": "serving_attempt_binding_mismatch", "requested_generation": json.Number("3"), "last_observed_generation": json.Number("3"), "observed_native_handoffs": json.Number("0"), "binding_predicate": "direct_physical_window_missing", "background_stage": "returned", "accepted_attempt_stage": "no_accepted_witness"}
	return status
}

func TestEditProbeSupersededLatestStatusDiscardsEarlierProof(t *testing.T) {
	for _, mode := range []string{"proven", "explicit_null", "accepted", "missing_P_observation", "malformed_B", "malformed_coalesced", "missing_handoff", "native_handoff", "foreign_refusal", "coalesced", "unavailable_status", "wrong_publication"} {
		t.Run(mode, func(t *testing.T) {
			status := supersededStatusFixture()
			raw, err := json.Marshal(stubContent(status))
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decodeEditProbeResult(raw)
			if err != nil {
				t.Fatal(err)
			}
			first := supersededStatus(decoded, raw, 34)
			if first == nil {
				t.Fatal("real decoded status did not supply candidate")
			}
			switch mode {
			case "accepted":
				decoded["phase_timing"].(map[string]any)["patch_wall_ms"] = json.Number("1")
			case "missing_P_observation":
				delete(decoded["phase_timing"].(map[string]any), "patch_wall_ms")
			case "explicit_null":
				decoded["requested_background"], decoded["coalesced_publication"] = nil, nil
			case "malformed_B":
				decoded["requested_background"] = map[string]any{}
			case "malformed_coalesced":
				decoded["coalesced_publication"] = "foreign"
			case "missing_handoff":
				delete(decoded["requested_background_unavailable"].(map[string]any), "observed_native_handoffs")
			case "native_handoff":
				decoded["requested_background_unavailable"].(map[string]any)["observed_native_handoffs"] = json.Number("1")
			case "foreign_refusal":
				decoded["requested_background_unavailable"].(map[string]any)["reason"] = "timer_callback_already_started"
			case "coalesced":
				decoded["applied_generation"] = json.Number("4")
			case "unavailable_status":
				decoded = nil
			case "wrong_publication":
				decoded["publication_phases"].(map[string]any)["source"] = "foreign"
			}
			latest := supersededStatus(decoded, raw, 35)
			if (latest != nil) != (mode == "proven" || mode == "explicit_null") {
				t.Fatalf("latest=%+v mode=%s", latest, mode)
			}
			if mode == "proven" && (latest.WireID != 35 || latest.SHA256 != fmt.Sprintf("%x", sha256.Sum256(raw))) {
				t.Fatal("status proof lost current wire identity")
			}
			if first.WireID != 34 {
				t.Fatal("earlier frozen scalar mutated")
			}
		})
	}
}

func TestEditProbeSupersededFinishJoinsRealLogsAfterEvidenceSettles(t *testing.T) {
	for _, mode := range []string{"proven", "missing_guard", "ambiguous_guard", "sink_error", "cap", "incorrect_read", "missing_file", "missing_call_chain", "read_error", "stale", "ack_error", "late_invalid_status", "accepted"} {
		t.Run(mode, func(t *testing.T) {
			record, proof := supersededRecordFixture(t)
			root := t.TempDir()
			data, err := json.Marshal(proof.Guard)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "ambiguous_guard" {
				data = append(append(data, '\n'), data...)
			}
			if mode != "missing_guard" {
				if err := os.WriteFile(filepath.Join(root, "daemon-1.stderr.log"), append(data, '\n'), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			status := supersededStatusFixture()
			raw, err := json.Marshal(stubContent(status))
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := decodeEditProbeResult(raw)
			if err != nil {
				t.Fatal(err)
			}
			record.Accounting = checkEditProbeAccounting(decoded, record.Receipt)
			record.Errors = []string{"A/P/B accounting unavailable: " + strings.Join(record.Accounting.Reasons, "; ")}
			record.supersededStatus = supersededStatus(decoded, raw, 34)
			if record.Accounting.A == nil || record.Accounting.State != "terminal_unavailable_background" || record.supersededStatus == nil {
				t.Fatalf("invalid real status/accounting fixture: %+v", record.Accounting)
			}
			state := &editProbeState{record: record, rawBytes: 98839}
			switch mode {
			case "sink_error":
				state.sinkErr = errors.New("raw sink write failed")
			case "cap":
				state.sinkErr = errEditProbeEvidenceIncomplete
			case "incorrect_read":
				state.record.Readbacks[0].Correct = false
			case "missing_file":
				state.record.Readbacks = append(state.record.Readbacks[:1], state.record.Readbacks[2:]...)
			case "missing_call_chain":
				delete(state.record.FirstCorrect, "call_chain")
			case "read_error":
				state.record.Readbacks[0].Error = "readback failed"
			case "stale":
				state.record.StaleClaimedFresh = true
			case "ack_error":
				state.record.Accounting.Reasons = append(state.record.Accounting.Reasons, "missing A phase disk_write")
				state.record.Errors[0] += "; missing A phase disk_write"
			case "late_invalid_status":
				state.record.supersededStatus = supersededStatus(nil, nil, 35)
			case "accepted":
				state.record.Accounting.P = &editProbeWindow{WallNS: 1}
			}
			phase := &Phase{Metrics: make(map[string]Metric), EditProbe: &editProbeReport{PhysicalWriteScope: &editProbePhysicalWriteScope{Kind: "shared_worktree_file", File: "sample.go", Editors: 4}}}
			runtime := &editProbeRuntime{p: &privateRun{Root: root, Repos: []privateRepo{{Prefix: "repo"}}}, phase: phase, states: []*editProbeState{state}, plan: editProbePlan{Steps: []editProbeStep{{Ordinal: 1}}, Seed: 1}, requireAccounting: true}
			err = runtime.finish()
			if (err == nil) != (mode == "proven") {
				t.Fatalf("finish error=%v mode=%s", err, mode)
			}
			got := phase.EditProbe.Records[0]
			if mode == "proven" {
				if got.Outcome != receiptproof.SupersededOutcome || got.Superseded == nil || got.Superseded.AccountingError != record.Errors[0] || got.Accounting.P != nil || got.Accounting.B != nil || len(got.Accounting.Reasons) == 0 || phase.EditProbe.Summary.Outcomes[receiptproof.SupersededOutcome] != 1 {
					t.Fatalf("finish erased refusal/count: %+v", got)
				}
				data, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(data), `"physical":null`) || !strings.Contains(string(data), `"requested_background":null`) {
					t.Fatal("null P/B became measured or hidden")
				}
			} else if got.Superseded != nil || got.Outcome == receiptproof.SupersededOutcome {
				t.Fatal("failed evidence got classified")
			}
		})
	}
}
