package main

import (
	"context"
	"encoding/json"
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
