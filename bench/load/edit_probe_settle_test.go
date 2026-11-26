package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// The single-edit receipt loop waits for terminal accounting like the
// one-file batch collector. A native-duplicate physical attempt whose earlier
// publisher is never reported keeps its accounting unavailable for good, so
// once nothing else is outstanding the loop gets the same bounded number of
// unchanged status cycles before it records the accounting gap. A qualified
// functional candidate waits for its live tiers, not for accounting, and is
// not bounded.
func TestEditProbeReceiptLoopStopsWhenAccountingStaysUnsettled(t *testing.T) {
	const n = editProbeAccountingSettlePolls
	for _, tc := range []struct {
		name string
		// The status read on which the graph turns fresh, the last
		// first-correct time is measured, the assigned post-ACK coverage
		// completes, the accounting changes (still unsettled), or a
		// qualified candidate's last live tier turns fresh.
		graphAt, lateAt, coverageAt, changeAt, tiersAt int
	}{
		{name: "observables_complete"},
		{name: "graph_late", graphAt: 2 * n},
		{name: "first_correct_late", lateAt: 3 * n},
		{name: "coverage_late", coverageAt: 2*n + 1},
		{name: "accounting_changes", changeAt: n - 1},
		{name: "qualified_tiers_late", tiersAt: 3 * n},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			c := editProbeCorpus()
			plan, err := makeEditProbePlan(c, 42, 1)
			if err != nil {
				t.Fatal(err)
			}
			step := plan.Steps[0]
			dir := t.TempDir()
			path := filepath.Join(dir, c.File)
			if err := os.WriteFile(path, []byte("package sample\n\n"+c.EditAnchor+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			status := editProbeRepairAccountingFixture()
			delete(status, "read_assisted_publication")
			if tc.tiersAt > 0 {
				_, status = functionalUnavailableRecordFixture(t, false)
				status["receipt"], status["applied_generation"] = "receipt-a", json.Number("4")
				status["tier_state"].(map[string]any)["derived"] = "pending"
			}
			status["mutation_id"], status["graph_status_terminal"], status["graph_status"] = step.MutationID, true, "fresh"
			fixture := (&editProbeRuntime{}).accountingForStatus(ctx, status, "receipt-a", plan.Tool, false)
			raw, err := json.Marshal(stubContent(status))
			if err != nil {
				t.Fatal(err)
			}
			qualified := coalescedFunctionalStatus(status, raw, 1, fixture) != nil || functionalUnavailableStatus(status, raw, 1, fixture) != nil
			if editProbeFinishedWithQualification(true, true, true, fixture, qualified, editProbeStatusTiers(status)) || qualified != (tc.tiersAt > 0) ||
				!qualified && (fixture.State != "unavailable" || !slices.Contains(fixture.Reasons, "actual publisher deep coverage unavailable")) {
				t.Fatalf("fixture is not the unsettled shape (qualified=%t): %+v", qualified, fixture)
			}
			if tc.graphAt > 0 {
				status["graph_status_terminal"], status["graph_status"] = false, "pending"
			}
			var mu sync.Mutex
			reads := 0
			var state *editProbeState
			disk := editProbeDiskStub(t, c, dir)
			run := newEditProbeStubRun(t, ctx, c, plan, dir, 1, func(frame stubFrame) (any, *rpcError) {
				args, _ := frame.Params["arguments"].(map[string]any)
				switch {
				case frame.Method == "tools/call" && frame.Params["name"] == "edit" && args["operation"] == "file":
					original, err := os.ReadFile(path)
					match, _ := args["match"].(string)
					replacement, _ := args["replacement"].(string)
					if err != nil || match == "" || strings.Count(string(original), match) != 1 {
						return nil, &rpcError{Code: -32000, Message: "controlled edit match missing or ambiguous"}
					}
					if err := os.WriteFile(path, []byte(strings.Replace(string(original), match, replacement, 1)), 0o600); err != nil {
						return nil, &rpcError{Code: -32000, Message: err.Error()}
					}
					return stubContent(map[string]any{"mutation_receipt": "receipt-a"}), nil
				case frame.Method == "tools/call" && frame.Params["name"] == "change" && args["operation"] == "receipt":
					// Serialized, so a change below is visible to this read and
					// every later one.
					mu.Lock()
					defer mu.Unlock()
					reads++
					switch reads {
					case tc.graphAt:
						status["graph_status_terminal"], status["graph_status"] = true, "fresh"
					case tc.lateAt:
						state.mu.Lock()
						state.record.FirstCorrect["text"] = measured(2, "ms")
						state.mu.Unlock()
					case tc.coverageAt:
						state.mu.Lock()
						state.record.Consumers["consumer_detect"] = editConsumerObservation{State: "unsupported"}
						state.mu.Unlock()
					case tc.changeAt:
						status["requested_background"].(map[string]any)["background_wall_ms"] = json.Number("13")
					case tc.tiersAt:
						status["tier_state"].(map[string]any)["derived"] = "fresh"
					}
					return stubContent(status), nil
				}
				return disk(frame)
			})
			run.options.Deadline = 2 * time.Second
			state = run.states[0]
			state.mu.Lock()
			if _, ok := state.record.FirstCorrect["text"]; !ok {
				state.mu.Unlock()
				t.Fatal("corpus has no text oracle")
			}
			for name := range state.record.FirstCorrect {
				state.record.FirstCorrect[name] = measured(1, "ms")
			}
			if tc.lateAt > 0 {
				state.record.FirstCorrect["text"] = unavailable("ms", "not yet observed")
			}
			if tc.coverageAt > 0 {
				// One relation consumer has no post-ACK answer yet.
				state.record.Consumers = map[string]editConsumerObservation{}
				for _, name := range editConsumerNames {
					state.record.Consumers[name] = editConsumerObservation{State: "unsupported"}
				}
				state.record.Consumers["consumer_detect"] = editConsumerObservation{State: "missing"}
			}
			started := time.Now()
			state.firstProbeWrite = started.Add(-time.Millisecond) // an original probe was on the wire first
			state.mu.Unlock()
			callCtx, stopCall := context.WithTimeout(ctx, run.options.Deadline)
			defer stopCall()
			ack, identity, err := run.perform(callCtx, state, run.editorFor(step), step.Request, "edit", "workload", 0)
			run.finishEditAfterACK(ctx, callCtx, state, step, started, ack, identity, err)
			mu.Lock()
			got := reads
			mu.Unlock()
			state.mu.Lock()
			record := state.record
			state.mu.Unlock()
			if callCtx.Err() != nil {
				t.Fatalf("receipt loop polled until the deadline (%d status cycles, outcome %s, reason %q) after every observable was complete", got, record.Outcome, record.DeadlineReason)
			}
			// Reads before the graph is fresh, before the last first-correct
			// time, before the post-ACK coverage, and before the last
			// accounting change do not count toward the bound. A qualified
			// candidate ends on the read that makes its tiers fresh.
			want := max(tc.graphAt, tc.lateAt, tc.coverageAt, tc.changeAt, 1) + n - 1
			if tc.tiersAt > 0 {
				want = tc.tiersAt
			}
			if got != want {
				t.Fatalf("status cycles = %d, want %d", got, want)
			}
			accounting := record.Accounting
			if !record.GraphTerminal || accounting.State != "unavailable" || editProbeAccountingComplete(accounting) ||
				slices.Contains(accounting.Reasons, editProbeAccountingUnsettledReason) != (tc.tiersAt == 0) {
				t.Fatalf("accounting did not stay unavailable with the stop reason only where the bound applies: %+v", accounting)
			}
			wantErrors := []string{"A/P/B accounting unavailable: " + strings.Join(accounting.Reasons, "; ")}
			if record.Outcome != "diagnostic_failure" || record.DeadlineReason != "" || !slices.Equal(record.Errors, wantErrors) {
				t.Fatalf("record = %s (deadline reason %q) %q, want diagnostic_failure with only the accounting gap", record.Outcome, record.DeadlineReason, record.Errors)
			}
		})
	}
}

// editProbeBatchSetSettleFixture is a committed two-file batch whose shared
// acknowledgement is reported but whose members' typed shared-owner proof is
// not, so its accounting stays unavailable_shared_owner however long the
// collector polls.
func editProbeBatchSetSettleFixture(t *testing.T) (editProbeBatchSetReferences, []editProbeBatchIntentMember, map[string]any, map[string]map[string]any) {
	t.Helper()
	root := filepath.Join(os.TempDir(), "edit-probe-owned-set")
	changes := []any{}
	for _, name := range []string{"a.go", "b.go"} {
		changes = append(changes, map[string]any{"op": "edit_file", "path": "repo/" + name, "old_string": "package fixture\n", "new_string": "package fixture\nfunc Added" + strings.ToUpper(name[:1]) + "() {}\n"})
	}
	offered := request{Tool: "edit", Operation: "batch", Arguments: map[string]any{"changes": changes, "options": map[string]any{"transaction_id": "transaction-set"}}}
	transaction, intents, err := editProbeBatchSetIntent(offered, root, "repo")
	if err != nil {
		t.Fatal(err)
	}
	var files, results, observations []any
	for i, intent := range intents {
		sum := sha256.Sum256(intent.Offered)
		receipt, generation := fmt.Sprintf("mutation-%d", 3+i), json.Number(fmt.Sprint(4+i))
		files = append(files, map[string]any{"path": intent.Path, "relative_path": intent.RelativePath, "after_sha256": hex.EncodeToString(sum[:]), "reindex_receipt": receipt, "reindex_generation": generation})
		results = append(results, map[string]any{"op": "edit_file", "path": intent.RelativePath, "status": "applied", "reindex_receipt": receipt, "reindex_generation": generation})
		observations = append(observations, map[string]any{"path": intent.RelativePath, "receipt": fmt.Sprintf("batch-file-%d", 5+i)})
	}
	fingerprint := sha256.Sum256([]byte(transaction))
	tx := map[string]any{"transaction_id": transaction, "status": "committed", "disk_status": "committed", "fingerprint": hex.EncodeToString(fingerprint[:]), "ack_mode": "local",
		"acknowledgement_receipt": "batch-ack-2", "acknowledgement_state": "reference_only_query_mutation_status", "files": files, "results": results, "file_observations": observations}
	refs, err := decodeEditProbeBatchSetAcknowledgement(tx, transaction, intents, "17")
	if err != nil || refs.validateReferences() != nil {
		t.Fatalf("batch set references: %v %v", err, refs.validateReferences())
	}
	receipts := map[string]map[string]any{refs.Acknowledgement: {"found": true, "receipt_kind": "batch_acknowledgement", "receipt": refs.Acknowledgement,
		"acknowledgement": map[string]any{"rpc_id": refs.RPCID, "clock": "monotonic", "scope": "shared_rpc_mcp_bound_call"}}}
	for _, file := range refs.Files {
		receipts[file.Observation] = map[string]any{"found": true, "receipt_kind": "batch_file_observation", "receipt": file.Observation, "transaction_id": file.TransactionID,
			"path": file.RelativePath, "batch_disk_verdict": "committed", "after_sha256": file.AfterSHA256, "publication_receipt": file.GraphReceipt,
			"publication_generation": json.Number(fmt.Sprint(file.GraphGeneration)), "writer_return_observed": true, "acknowledgement_state": "not_applicable_shared_transaction_rpc",
			"shared_owner_accounting_v1_availability": map[string]any{"state": "pending"}}
	}
	tx["graph_status"] = "fresh"
	return refs, intents, tx, receipts
}

// The multi-file batch collector has its own shared-owner accounting and the
// same bound: unchanged unsettled accounting ends the poll once the graph is
// fresh and every first-correct time is measured.
func TestEditProbeBatchSetCollectorStopsWhenAccountingStaysUnsettled(t *testing.T) {
	const n = editProbeAccountingSettlePolls
	for _, tc := range []struct {
		name string
		// The transaction read on which the graph turns fresh, the last
		// first-correct time is measured, or the accounting changes (still
		// unsettled).
		graphAt, lateAt, changeAt int
	}{
		{name: "observables_complete"},
		{name: "graph_late", graphAt: 2 * n},
		{name: "first_correct_late", lateAt: 3 * n},
		{name: "accounting_changes", changeAt: n - 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			refs, intents, tx, receipts := editProbeBatchSetSettleFixture(t)
			statuses := map[string]map[string]any{}
			for _, file := range refs.Files {
				statuses[file.Observation] = receipts[file.Observation]
			}
			fixture, batch := (&editProbeRuntime{}).accountingForBatchSetStatuses(ctx, statuses, receipts[refs.Acknowledgement], refs)
			proofPending := slices.ContainsFunc(fixture.Reasons, func(reason string) bool {
				return strings.HasSuffix(reason, "complete typed shared owner proof unavailable")
			})
			if fixture.State != "unavailable_shared_owner" || batch.State != "unavailable_batch_shared_owner" || editProbeFinished(true, true, true, fixture) || !proofPending {
				t.Fatalf("fixture is not the unsettled shared-owner shape: %+v %+v", fixture, batch)
			}
			if tc.graphAt > 0 {
				tx["graph_status"] = "pending"
			}
			c := editProbeCorpus()
			plan, err := makeEditProbePlan(c, 42, 1)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, c.File), []byte("package sample\n\n"+c.EditAnchor+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			reads := 0
			var state *editProbeState
			run := newEditProbeStubRun(t, ctx, c, plan, dir, 1, func(frame stubFrame) (any, *rpcError) {
				args, _ := frame.Params["arguments"].(map[string]any)
				options, _ := args["options"].(map[string]any)
				// Serialized, so a change below is visible to this cycle's
				// reads and every later one.
				mu.Lock()
				defer mu.Unlock()
				if frame.Params["name"] == "edit" {
					reads++
					switch reads {
					case tc.graphAt:
						tx["graph_status"] = "fresh"
					case tc.lateAt:
						state.mu.Lock()
						state.record.FirstCorrect["text"] = measured(2, "ms")
						state.mu.Unlock()
					case tc.changeAt:
						receipts[refs.Files[1].Observation]["writer_return_observed"] = false
					}
					return stubContent(tx), nil
				}
				receipt, _ := options["receipt"].(string)
				return stubContent(receipts[receipt]), nil
			})
			state = run.states[0]
			first := map[string]Metric{"source": measured(1, "ms"), "text": measured(1, "ms")}
			if tc.lateAt > 0 {
				first["text"] = unavailable("ms", "not yet observed")
			}
			state.mu.Lock()
			state.record.FirstCorrect = first
			state.mu.Unlock()
			run.collectBatchSetEvidence(ctx, state, refs, intents)
			mu.Lock()
			got := reads
			mu.Unlock()
			state.mu.Lock()
			record := state.record
			state.mu.Unlock()
			if ctx.Err() != nil {
				t.Fatalf("batch set collector polled until the deadline (%d status cycles) after every observable was complete", got)
			}
			if want := max(tc.graphAt, tc.lateAt, tc.changeAt, 1) + n - 1; got != want {
				t.Fatalf("status cycles = %d, want %d", got, want)
			}
			accounting := record.Accounting
			if !record.GraphTerminal || accounting.State != "unavailable_shared_owner" || editProbeAccountingComplete(accounting) || !slices.Contains(accounting.Reasons, editProbeAccountingUnsettledReason) ||
				record.BatchAccounting == nil || record.BatchAccounting.State != "unavailable_batch_shared_owner" || !slices.Contains(record.BatchAccounting.Reasons, editProbeAccountingUnsettledReason) {
				t.Fatalf("unsettled accounting was not kept unavailable with its stop reason: %+v %+v", accounting, record.BatchAccounting)
			}
			if len(record.Errors) != 0 {
				t.Fatalf("collector added errors: %q", record.Errors)
			}
		})
	}
}
