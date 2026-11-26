package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func editProbeBatchACKAbsPath() string {
	return filepath.Join(os.TempDir(), "edit-probe-owned-repo", "file.go")
}
func editProbeBatchOfferedBytes() []byte {
	return []byte("package fixture\nfunc Added() { Callee() }\n")
}
func editProbeBatchOfferedSHA() string {
	hash := sha256.Sum256(editProbeBatchOfferedBytes())
	return hex.EncodeToString(hash[:])
}

func editProbeBatchACKFixture() map[string]any {
	return map[string]any{"file_observations": []any{map[string]any{"path": "repo/file.go", "receipt": "batch-file-5"}}, "transaction_id": "transaction-1", "status": "committed", "disk_status": "committed", "acknowledgement_receipt": "batch-ack-2", "acknowledgement_state": "reference_only_query_mutation_status", "files": []any{map[string]any{"path": editProbeBatchACKAbsPath(), "relative_path": "repo/file.go", "after_sha256": editProbeBatchOfferedSHA(), "reindex_receipt": "mutation-3", "reindex_generation": json.Number("4")}}, "results": []any{map[string]any{"op": "edit_file", "path": "repo/file.go", "status": "applied", "reindex_receipt": "mutation-3", "reindex_generation": json.Number("4")}}}
}

func TestEditProbeBatchKeepsActualTransactionACKAndFileIdentities(t *testing.T) {
	refs, err := decodeEditProbeBatchAcknowledgement(editProbeBatchACKFixture(), "transaction-1", editProbeBatchACKAbsPath(), "repo/file.go", editProbeBatchOfferedBytes())
	if err != nil || refs.validateReferences() != nil {
		t.Fatal(refs, err)
	}
	if refs.TransactionID != "transaction-1" || refs.Acknowledgement != "batch-ack-2" || refs.GraphReceipt != "mutation-3" || refs.GraphGeneration != 4 || refs.AfterSHA256 != editProbeBatchOfferedSHA() {
		t.Fatal(refs)
	}
}

func TestEditProbeBatchRejectsWrongSourceVerdictOrFileJoin(t *testing.T) {
	for _, name := range []string{"transaction", "rollback", "pending", "files", "results", "wrong_path", "wrong_relative", "wrong_result_path", "wrong_op", "failed_result", "absent", "malformed_absent", "hash", "foreign_after_hash", "generation"} {
		t.Run(name, func(t *testing.T) {
			body := editProbeBatchACKFixture()
			file := body["files"].([]any)[0].(map[string]any)
			result := body["results"].([]any)[0].(map[string]any)
			switch name {
			case "transaction":
				body["transaction_id"] = "foreign"
			case "rollback":
				body["disk_status"] = "rolled_back"
			case "pending":
				body["status"] = "prepared"
			case "files":
				body["files"] = []any{file, file}
			case "results":
				body["results"] = []any{}
			case "wrong_path":
				file["path"] = "/foreign/file.go"
			case "wrong_relative":
				file["relative_path"] = "foreign/file.go"
			case "wrong_result_path":
				result["path"] = "foreign/file.go"
			case "wrong_op":
				result["op"] = "edit_symbol"
			case "failed_result":
				result["status"] = "failed"
			case "absent":
				file["after_absent"] = true
			case "malformed_absent":
				file["after_absent"] = "false"
			case "hash":
				file["after_sha256"] = strings.Repeat("b", 40)
			case "foreign_after_hash":
				file["after_sha256"] = strings.Repeat("b", 64)
			case "generation":
				file["reindex_generation"] = json.Number("1.5")
			}
			if _, err := decodeEditProbeBatchAcknowledgement(body, "transaction-1", editProbeBatchACKAbsPath(), "repo/file.go", editProbeBatchOfferedBytes()); err == nil {
				t.Fatal("accepted contradictory durable ACK", name)
			}
		})
	}
}

func TestEditProbeBatchMissingReferencesDoNotEraseCommittedACK(t *testing.T) {
	for _, name := range []string{"missing_A", "missing_graph", "wrong_result_graph", "wrong_result_generation", "swapped_domains"} {
		t.Run(name, func(t *testing.T) {
			body := editProbeBatchACKFixture()
			file := body["files"].([]any)[0].(map[string]any)
			result := body["results"].([]any)[0].(map[string]any)
			switch name {
			case "missing_A":
				delete(body, "acknowledgement_receipt")
			case "missing_graph":
				delete(file, "reindex_receipt")
				delete(file, "reindex_generation")
			case "wrong_result_graph":
				result["reindex_receipt"] = "foreign"
			case "wrong_result_generation":
				result["reindex_generation"] = json.Number("5")
			case "swapped_domains":
				file["reindex_receipt"] = "batch-ack-2"
				result["reindex_receipt"] = "batch-ack-2"
			}
			refs, err := decodeEditProbeBatchAcknowledgement(body, "transaction-1", editProbeBatchACKAbsPath(), "repo/file.go", editProbeBatchOfferedBytes())
			if err != nil {
				t.Fatal("lost genuine committed source ACK", err)
			}
			if refs.validateReferences() == nil {
				t.Fatal("accepted unavailable or foreign accounting", refs)
			}
		})
	}
}

func TestEditProbeBatchRequestUsesPublicChangesAndTransactionOption(t *testing.T) {
	c := Corpus{File: "file.go", EditAnchor: "// anchor\n", EditTemplate: "func {{marker}}() { Callee() }\n"}
	before := "package fixture\n// anchor\n"
	r, after := editProbeBatchRequest(c, before, "Marker", "transaction-1")
	if r.name() != "edit.batch" {
		t.Fatal(r)
	}
	expectedAfter := "package fixture\n// anchor\nfunc Marker() { Callee() }\n"
	expected := map[string]any{"operation": "batch", "changes": []any{map[string]any{"op": "edit_file", "path": "file.go", "old_string": before, "new_string": expectedAfter}}, "options": map[string]any{"transaction_id": "transaction-1"}}
	if after != expectedAfter {
		t.Fatal("changed offered intent", after)
	}
	if !reflect.DeepEqual(r.Arguments, expected) {
		t.Fatal("wrong public batch wire shape", r.Arguments)
	}
	if _, present := r.Arguments["mutation_id"]; present {
		t.Fatal("substituted single-edit identity")
	}
}

func TestEditProbeBatchFileObservationRequiresOriginalDurableIdentity(t *testing.T) {
	refs, err := decodeEditProbeBatchAcknowledgement(editProbeBatchACKFixture(), "transaction-1", editProbeBatchACKAbsPath(), "repo/file.go", editProbeBatchOfferedBytes())
	if err != nil {
		t.Fatal(err)
	}
	fixture := func() map[string]any {
		return map[string]any{"found": true, "receipt_kind": "batch_file_observation", "receipt": "batch-file-5", "transaction_id": "transaction-1", "path": "repo/file.go", "batch_disk_verdict": "committed", "after_sha256": editProbeBatchOfferedSHA(), "publication_receipt": "mutation-3", "publication_generation": json.Number("4"), "writer_return_observed": true, "acknowledgement_state": "not_applicable_shared_transaction_rpc"}
	}
	if err := refs.validateFileObservation(fixture()); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"receipt", "transaction_id", "path", "after_sha256", "publication_receipt", "publication_generation", "batch_disk_verdict", "writer_return_observed", "receipt_kind", "found", "acknowledgement_state"} {
		t.Run(key, func(t *testing.T) {
			body := fixture()
			delete(body, key)
			if refs.validateFileObservation(body) == nil {
				t.Fatal("accepted missing identity", key)
			}
		})
	}
	for _, foreign := range []string{refs.TransactionID, refs.Acknowledgement, refs.GraphReceipt, "batch-file-other"} {
		body := fixture()
		body["receipt"] = foreign
		if refs.validateFileObservation(body) == nil {
			t.Fatal("borrowed receipt", foreign)
		}
	}
}

func TestEditProbeBatchAccountingKeepsSharedACKOutsideFileAndWinnerWindows(t *testing.T) {
	refs, err := decodeEditProbeBatchAcknowledgement(editProbeBatchACKFixture(), "transaction-1", editProbeBatchACKAbsPath(), "repo/file.go", editProbeBatchOfferedBytes())
	if err != nil {
		t.Fatal(err)
	}
	refs.GraphGeneration, refs.ResultGeneration = 1, 1
	file := editProbeAccountingFixture()
	shared := map[string]any{"found": true, "receipt": refs.Acknowledgement, "receipt_kind": "batch_acknowledgement", "acknowledgement": file["acknowledgement"]}
	file["acknowledgement"] = map[string]any{"rpc_id": "foreign-file-RPC"}
	file["found"], file["receipt_kind"], file["receipt"] = true, "batch_file_observation", refs.Observation
	file["transaction_id"], file["path"], file["batch_disk_verdict"] = refs.TransactionID, refs.RelativePath, "committed"
	file["after_sha256"], file["publication_receipt"], file["publication_generation"] = refs.AfterSHA256, refs.GraphReceipt, json.Number("1")
	file["writer_return_observed"], file["acknowledgement_state"] = true, "not_applicable_shared_transaction_rpc"
	got := refs.accounting(file, shared)
	if got.State != "observed_single_window_checks_pass" || got.RPCID != "rpc-a" || got.A == nil || got.P == nil || got.B == nil {
		t.Fatalf("independent windows unavailable: %+v", got)
	}
	for _, key := range []string{"receipt", "receipt_kind", "found", "acknowledgement"} {
		copy := map[string]any{}
		for k, v := range shared {
			copy[k] = v
		}
		delete(copy, key)
		if refs.accounting(file, copy).State == "observed_single_window_checks_pass" {
			t.Fatal("accepted missing shared ACK owner", key)
		}
	}
	if file["acknowledgement"].(map[string]any)["rpc_id"] != "foreign-file-RPC" {
		t.Fatal("mutated/merged original file status")
	}
}

func TestEditProbeBatchPlanKeepsOfferedPrefixAndScheduleAcrossFixtureLanguages(t *testing.T) {
	for _, language := range []string{"go", "typescript", "python"} {
		t.Run(language, func(t *testing.T) {
			c := editProbeWriteCorpus(t, language)
			write, err := makeEditProbePlan(c, 42, 100)
			if err != nil {
				t.Fatal(err)
			}
			c.EditTool = "edit.batch"
			plan, err := makeEditProbePlan(c, 42, 100)
			if err != nil {
				t.Fatal(err)
			}
			if plan.Tool != "edit.batch" || len(plan.Steps) != 100 {
				t.Fatal("wrong tool/denominator")
			}
			prior := c.EditInitialContent
			for i, step := range plan.Steps {
				id, offered, err := editProbeBatchIntent(step.Request)
				if err != nil {
					t.Fatal(err)
				}
				if id == step.MutationID || id == "" {
					t.Fatal("borrowed single mutation identity")
				}
				if step.DueNS != write.Steps[i].DueNS || step.ActivateNS != write.Steps[i].ActivateNS || step.Marker != write.Steps[i].Marker {
					t.Fatal("tool changed offered traffic")
				}
				if string(offered) != write.Steps[i].Request.Arguments["content"] {
					t.Fatal("batch regenerated different offered bytes")
				}
				change := step.Request.Arguments["changes"].([]any)[0].(map[string]any)
				if change["old_string"] != prior {
					t.Fatal("batch does not require exact prior offered prefix")
				}
				prior = string(offered)
				args := dispatchArgs(step.Request, privateRepo{Prefix: "owned-repo"})
				wire := args["changes"].([]any)[0].(map[string]any)
				if wire["path"] != "owned-repo/"+c.File || change["path"] != c.File {
					t.Fatal("wrong actual scoped path or mutated immutable plan")
				}
			}
			if err := editProbeWriteAdmission(c, []byte(c.EditInitialContent+"unexpected")); err == nil {
				t.Fatal("accepted different initial bytes")
			}
		})
	}
}

// editProbeBatchCollectorStub serves the three public owners a one-file batch
// collector reads: the fresh transaction, the file observation built from an
// ordinary receipt-status fixture, and the shared acknowledgement.
type editProbeBatchCollectorStub struct {
	refs             editProbeBatchReferences
	tx, file, shared map[string]any
	// onTransaction runs inside the stub, before any later status read is
	// served, on each transaction status read with its 1-based count.
	onTransaction func(int)
}

func newEditProbeBatchCollectorStub(t *testing.T, status map[string]any) *editProbeBatchCollectorStub {
	t.Helper()
	refs, err := decodeEditProbeBatchAcknowledgement(editProbeBatchACKFixture(), "transaction-1", editProbeBatchACKAbsPath(), "repo/file.go", editProbeBatchOfferedBytes())
	if err != nil {
		t.Fatal(err)
	}
	refs.GraphGeneration, refs.ResultGeneration = 1, 1
	tx := editProbeBatchACKFixture()
	tx["graph_status"] = "fresh"
	tx["files"].([]any)[0].(map[string]any)["reindex_generation"] = json.Number("1")
	tx["results"].([]any)[0].(map[string]any)["reindex_generation"] = json.Number("1")
	shared := map[string]any{"found": true, "receipt": refs.Acknowledgement, "receipt_kind": "batch_acknowledgement", "acknowledgement": status["acknowledgement"]}
	delete(status, "acknowledgement")
	status["found"], status["receipt_kind"], status["receipt"] = true, "batch_file_observation", refs.Observation
	status["transaction_id"], status["path"], status["batch_disk_verdict"] = refs.TransactionID, refs.RelativePath, "committed"
	status["after_sha256"], status["publication_receipt"], status["publication_generation"] = refs.AfterSHA256, refs.GraphReceipt, json.Number("1")
	status["writer_return_observed"], status["acknowledgement_state"] = true, "not_applicable_shared_transaction_rpc"
	return &editProbeBatchCollectorStub{refs: refs, tx: tx, file: status, shared: shared}
}

// collect runs the actual collector against a stub daemon and returns the
// owner each status read named, in order.
func (f *editProbeBatchCollectorStub) collect(ctx context.Context, t *testing.T, state *editProbeState) []string {
	t.Helper()
	var mu sync.Mutex
	var owners []string
	transactions := 0
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		args, _ := frame.Params["arguments"].(map[string]any)
		options, _ := args["options"].(map[string]any)
		mu.Lock()
		defer mu.Unlock()
		var owner string
		var response map[string]any
		if frame.Params["name"] == "edit" {
			owner, _ = options["transaction_id"].(string)
			transactions++
			if f.onTransaction != nil {
				f.onTransaction(transactions)
			}
			response = f.tx
		} else {
			owner, _ = options["receipt"].(string)
			switch owner {
			case f.refs.Observation:
				response = f.file
			case f.refs.Acknowledgement:
				response = f.shared
			}
		}
		owners = append(owners, owner)
		return stubContent(response), nil
	})
	client, err := openMCP(ctx, socket, "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sink, err := os.CreateTemp(t.TempDir(), "batch-evidence-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer sink.Close()
	state.client, state.sink = client, sink
	state.startRaw()
	run := &editProbeRuntime{options: runOptions{Deadline: time.Second}, p: &privateRun{Repos: []privateRepo{{Prefix: "repo"}}}, phase: &Phase{Metrics: map[string]Metric{}}, origin: time.Now(), requireAccounting: true}
	run.collectBatchEvidence(ctx, state, f.refs, editProbeBatchOfferedBytes())
	state.finishRaw()
	mu.Lock()
	defer mu.Unlock()
	return owners
}

func TestEditProbeBatchCollectorReadsThreeIndependentPublicOwners(t *testing.T) {
	stub := newEditProbeBatchCollectorStub(t, editProbeAccountingFixture())
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	state := &editProbeState{record: editProbeRecord{FirstCorrect: map[string]Metric{"oracle": measured(1, "ms")}}}
	owners := stub.collect(ctx, t, state)
	if ctx.Err() != nil || !state.record.GraphTerminal || state.record.Accounting.State != "observed_single_window_checks_pass" {
		t.Fatalf("actual collector failed: %+v %v", state.record, ctx.Err())
	}
	if refs := stub.refs; !reflect.DeepEqual(owners, []string{refs.TransactionID, refs.Observation, refs.Acknowledgement}) {
		t.Fatal("owner route mismatch", owners)
	}
	if state.record.Receipt != "" {
		t.Fatal("invented single-file mutation receipt")
	}
}

// The fresh batch shape that exhausted the per-edit raw cap: every
// first-correct time is measured, but the physical attempt is a native
// duplicate whose earlier publisher is never reported, so the accounting is
// not terminal however long the collector waits.
func TestEditProbeBatchCollectorStopsWhenAccountingStaysUnsettled(t *testing.T) {
	for _, tc := range []struct {
		name string
		// The transaction read on which the last first-correct time is
		// measured, on which the accounting changes (still unsettled), or on
		// which the graph is fresh again after reads reported it pending.
		lateAt, changeAt, freshAt int
		// The first transaction read that reports the graph pending.
		pendingAt int
	}{
		{name: "observables_complete"},
		{name: "first_correct_late", lateAt: 3 * editProbeAccountingSettlePolls},
		{name: "accounting_changes", changeAt: editProbeAccountingSettlePolls - 1},
		// Counted cycles before the graph turns pending make the reset
		// observable: a skipped cycle would keep the count instead.
		{name: "graph_pending_then_fresh", pendingAt: editProbeAccountingSettlePolls / 2, freshAt: 2 * editProbeAccountingSettlePolls},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := editProbeRepairAccountingFixture()
			delete(status, "read_assisted_publication")
			stub := newEditProbeBatchCollectorStub(t, status)
			if got := stub.refs.accounting(stub.file, stub.shared); got.State != "unavailable" || !slices.Contains(got.Reasons, "actual publisher deep coverage unavailable") || editProbeFinished(true, true, true, got) {
				t.Fatalf("fixture is not the unsettled duplicate shape: %+v", got)
			}
			first := measured(1, "ms")
			if tc.lateAt > 0 {
				first = unavailable("ms", "not yet observed")
			}
			state := &editProbeState{record: editProbeRecord{FirstCorrect: map[string]Metric{"source": measured(1, "ms"), "text": first}}}
			stub.onTransaction = func(count int) {
				switch count {
				case tc.lateAt:
					state.mu.Lock()
					state.record.FirstCorrect["text"] = measured(2, "ms")
					state.mu.Unlock()
				case tc.changeAt:
					stub.file["requested_background"].(map[string]any)["background_wall_ms"] = json.Number("13")
				case tc.pendingAt:
					stub.tx["graph_status"] = "pending"
				case tc.freshAt:
					stub.tx["graph_status"] = "fresh"
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			owners := stub.collect(ctx, t, state)
			reads := 0
			for _, owner := range owners {
				if owner == stub.refs.TransactionID {
					reads++
				}
			}
			if ctx.Err() != nil {
				t.Fatalf("collector polled until the deadline (%d status cycles, %d raw bytes) after every observable was complete", reads, state.rawBytes)
			}
			// Reads before the last first-correct time, before the last
			// accounting change, and before the graph is fresh again do not
			// count toward the bound.
			if want := max(tc.lateAt, tc.changeAt, tc.freshAt, 1) + editProbeAccountingSettlePolls - 1; reads != want {
				t.Fatalf("status cycles = %d, want %d", reads, want)
			}
			got := state.record.Accounting
			if !state.record.GraphTerminal || got.State != "unavailable" || editProbeAccountingComplete(got) ||
				!slices.Contains(got.Reasons, "actual publisher deep coverage unavailable") || !slices.Contains(got.Reasons, editProbeAccountingUnsettledReason) {
				t.Fatalf("unsettled accounting was not kept unavailable with its stop reason: %+v", got)
			}
			if state.sinkErr != nil || state.rawBytes == 0 {
				t.Fatalf("raw evidence incomplete or missing: bytes=%d err=%v", state.rawBytes, state.sinkErr)
			}
		})
	}
}

func TestBatchAccountingTextSpliceParseApplicabilityIsExplicitAndOwned(t *testing.T) {
	base := editProbeAccountingFixture()
	ack := base["acknowledgement"].(map[string]any)
	ack["phase_states"].(map[string]any)["parse"] = "not_applicable"
	ack["observed_execution_ms"].(map[string]any)["parse"] = nil
	ack["parse_scope"] = "not_applicable_text_splice"
	ack["caller_response_ready_ms"] = json.Number("7")
	if got := checkEditProbeAccountingWithAcknowledgement(base, "receipt-a", "reindex_generation", ack, true); got.State != "observed_single_window_checks_pass" {
		t.Fatal("explicit unexecuted batch parse not honored", got)
	}
	if got := checkEditProbeAccounting(base, "receipt-a"); got.State == "observed_single_window_checks_pass" {
		t.Fatal("weakened ordinary applicable parse")
	}
	for _, name := range []string{"missing_scope", "unknown_scope", "missing_state", "unknown_state", "missing_value", "zero_instead_of_null"} {
		t.Run(name, func(t *testing.T) {
			raw, _ := json.Marshal(ack)
			var copy map[string]any
			decoder := json.NewDecoder(strings.NewReader(string(raw)))
			decoder.UseNumber()
			if err := decoder.Decode(&copy); err != nil {
				t.Fatal(err)
			}
			switch name {
			case "missing_scope":
				delete(copy, "parse_scope")
			case "unknown_scope":
				copy["parse_scope"] = "unknown"
			case "missing_state":
				delete(copy["phase_states"].(map[string]any), "parse")
			case "unknown_state":
				copy["phase_states"].(map[string]any)["parse"] = "unknown"
			case "missing_value":
				delete(copy["observed_execution_ms"].(map[string]any), "parse")
			case "zero_instead_of_null":
				copy["observed_execution_ms"].(map[string]any)["parse"] = json.Number("0")
			}
			if got := checkEditProbeAccountingWithAcknowledgement(base, "receipt-a", "reindex_generation", copy, true); got.State == "observed_single_window_checks_pass" {
				t.Fatal("invented batch parse coverage", name)
			}
		})
	}
}

func directRequestedBackgroundFixture() map[string]any {
	status := editProbeAccountingFixture()
	winner := status["coalesced_publication"].(map[string]any)
	phase := winner["timings"].(map[string]any)["phase_timing"].(map[string]any)
	phase["patch_accounting"].(map[string]any)["attempt_ticket"] = json.Number("1")
	status["phase_timing"] = phase
	delete(status, "coalesced_publication")
	status["applied_generation"] = json.Number("1")
	status["publication_phases"] = map[string]any{"key": "original", "ticket": json.Number("1"), "source": "mcp_edit", "clock": "monotonic"}
	background := status["requested_background"].(map[string]any)
	background["coverage"], background["observed_native_handoffs"] = "direct_requested_attempt", json.Number("0")
	background["applied_generation"] = json.Number("1")
	phases := background["phases_ms"].(map[string]any)
	for _, name := range []string{"canceled_quiet_wait", "timer_stop", "coalescing_control", "successor_timer_arm", "successor_quiet_wait"} {
		phases[name] = json.Number("0")
	}
	phases["requested_quiet_wait"] = json.Number("1")
	background["background_wall_ms"] = json.Number("16")
	return status
}
func directRequestedBackgroundWithPredecessorFixture() map[string]any {
	status := directRequestedBackgroundFixture()
	status["reindex_generation"], status["applied_generation"] = json.Number("3"), json.Number("3")
	status["publication_phases"].(map[string]any)["ticket"] = json.Number("3")
	status["phase_timing"].(map[string]any)["patch_accounting"].(map[string]any)["attempt_ticket"] = json.Number("3")
	b := status["requested_background"].(map[string]any)
	b["requested_generation"], b["applied_generation"] = json.Number("3"), json.Number("3")
	b["admission_predecessor_generation"], b["admission_predecessor_stopped"] = json.Number("2"), true
	return status
}
func TestDirectRequestedBackgroundConsumerRequiresExactOwnedWindow(t *testing.T) {
	if got := checkEditProbeAccounting(directRequestedBackgroundFixture(), "receipt-a"); got.State != "observed_single_window_checks_pass" {
		t.Fatal("valid direct B refused", got)
	}
	if got := checkEditProbeAccounting(directRequestedBackgroundWithPredecessorFixture(), "receipt-a"); got.State != "observed_single_window_checks_pass" {
		t.Fatal("valid actual predecessor proof refused", got)
	}
	for _, name := range []string{"missing_quiet", "borrowed_native_quiet", "wrong_applied", "fake_handoff", "unknown_shape", "predecessor_stop_false", "foreign_predecessor", "malformed_predecessor", "missing_stop_proof"} {
		t.Run(name, func(t *testing.T) {
			status := directRequestedBackgroundFixture()
			if strings.Contains(name, "predecessor") || name == "missing_stop_proof" {
				status = directRequestedBackgroundWithPredecessorFixture()
			}
			b := status["requested_background"].(map[string]any)
			phases := b["phases_ms"].(map[string]any)
			switch name {
			case "missing_quiet":
				delete(phases, "requested_quiet_wait")
			case "borrowed_native_quiet":
				phases["successor_quiet_wait"] = json.Number("1")
			case "wrong_applied":
				b["applied_generation"] = json.Number("2")
			case "fake_handoff":
				b["observed_native_handoffs"] = json.Number("1")
			case "unknown_shape":
				b["coverage"] = "unknown"
			case "predecessor_stop_false":
				b["admission_predecessor_stopped"] = false
			case "foreign_predecessor":
				b["admission_predecessor_generation"] = json.Number("3")
			case "malformed_predecessor":
				b["admission_predecessor_generation"] = "not-a-generation"
			case "missing_stop_proof":
				delete(b, "admission_predecessor_stopped")
			}
			if got := checkEditProbeAccounting(status, "receipt-a"); got.State == "observed_single_window_checks_pass" {
				t.Fatal("accepted contradictory direct B", name)
			}
		})
	}
}

func TestCoalescedBackgroundCannotBorrowAnExecutedRequestedQuietLap(t *testing.T) {
	status := editProbeAccountingFixture()
	b := status["requested_background"].(map[string]any)
	b["phases_ms"].(map[string]any)["requested_quiet_wait"] = json.Number("0")
	if got := checkEditProbeAccounting(status, "receipt-a"); got.State != "observed_single_window_checks_pass" {
		t.Fatal("new zero lap broke historical coalesced shape", got)
	}
	b["phases_ms"].(map[string]any)["requested_quiet_wait"] = json.Number("1")
	if editProbeSupportedBackgroundCoverage(b) {
		t.Fatal("coalesced shape borrowed an already-running requested callback")
	}
}
