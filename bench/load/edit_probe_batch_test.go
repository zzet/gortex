package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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

func TestEditProbeBatchCollectorReadsThreeIndependentPublicOwners(t *testing.T) {
	refs, err := decodeEditProbeBatchAcknowledgement(editProbeBatchACKFixture(), "transaction-1", editProbeBatchACKAbsPath(), "repo/file.go", editProbeBatchOfferedBytes())
	if err != nil {
		t.Fatal(err)
	}
	refs.GraphGeneration, refs.ResultGeneration = 1, 1
	tx := editProbeBatchACKFixture()
	tx["graph_status"] = "fresh"
	tx["files"].([]any)[0].(map[string]any)["reindex_generation"] = json.Number("1")
	tx["results"].([]any)[0].(map[string]any)["reindex_generation"] = json.Number("1")
	file := editProbeAccountingFixture()
	shared := map[string]any{"found": true, "receipt": refs.Acknowledgement, "receipt_kind": "batch_acknowledgement", "acknowledgement": file["acknowledgement"]}
	delete(file, "acknowledgement")
	file["found"], file["receipt_kind"], file["receipt"] = true, "batch_file_observation", refs.Observation
	file["transaction_id"], file["path"], file["batch_disk_verdict"] = refs.TransactionID, refs.RelativePath, "committed"
	file["after_sha256"], file["publication_receipt"], file["publication_generation"] = refs.AfterSHA256, refs.GraphReceipt, json.Number("1")
	file["writer_return_observed"], file["acknowledgement_state"] = true, "not_applicable_shared_transaction_rpc"
	var mu sync.Mutex
	var owners []string
	socket := startStub(t, func(frame stubFrame) (any, *rpcError) {
		args, _ := frame.Params["arguments"].(map[string]any)
		options, _ := args["options"].(map[string]any)
		var owner string
		var response map[string]any
		if frame.Params["name"] == "edit" {
			owner, _ = options["transaction_id"].(string)
			response = tx
		} else {
			owner, _ = options["receipt"].(string)
			if owner == refs.Observation {
				response = file
			} else if owner == refs.Acknowledgement {
				response = shared
			}
		}
		mu.Lock()
		owners = append(owners, owner)
		mu.Unlock()
		return stubContent(response), nil
	})
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
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
	state := &editProbeState{client: client, sink: sink, record: editProbeRecord{FirstCorrect: map[string]Metric{"oracle": measured(1, "ms")}}}
	state.startRaw()
	defer state.finishRaw()
	run := &editProbeRuntime{options: runOptions{Deadline: time.Second}, p: &privateRun{Repos: []privateRepo{{Prefix: "repo"}}}, phase: &Phase{Metrics: map[string]Metric{}}, origin: time.Now(), requireAccounting: true}
	run.collectBatchEvidence(ctx, state, refs, editProbeBatchOfferedBytes())
	if ctx.Err() != nil || !state.record.GraphTerminal || state.record.Accounting.State != "observed_single_window_checks_pass" {
		t.Fatalf("actual collector failed: %+v %v", state.record, ctx.Err())
	}
	mu.Lock()
	defer mu.Unlock()
	if !reflect.DeepEqual(owners, []string{refs.TransactionID, refs.Observation, refs.Acknowledgement}) {
		t.Fatal("owner route mismatch", owners)
	}
	if state.record.Receipt != "" {
		t.Fatal("invented single-file mutation receipt")
	}
}
