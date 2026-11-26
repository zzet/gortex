package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// This is a collector transport control, not product indexing evidence. The
// edit writes real fixture bytes before its ACK is delivered. Original probes
// can therefore be correct while still being ineligible post-ACK witnesses.
func TestEditProbeGateCompletionWaitsForPostACKCoverage(t *testing.T) {
	for _, mode := range []string{"release", "deadline", "cancel", "missing_winner", "accounting_deadline"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			corpus := editProbeCorpus()
			plan, err := makeEditProbePlan(corpus, 42, 1)
			if err != nil {
				t.Fatal(err)
			}
			plan.GateModes, plan.Steps[0].ProbeMode = true, "json"
			step := plan.Steps[0]
			receipt := "receipt-a"
			var terminalStatus map[string]any
			if mode == "missing_winner" {
				_, terminalStatus = functionalUnavailableRecordFixture(t, false)
				terminalStatus["receipt"], terminalStatus["mutation_id"] = receipt, step.MutationID
				terminalStatus["applied_generation"] = json.Number("4")
			}
			root := t.TempDir()
			path := filepath.Join(root, corpus.File)
			if err := os.WriteFile(path, []byte("package fixture\nfunc Example() int { return 1 }\n"+corpus.EditAnchor+"\n"), 0600); err != nil {
				t.Fatal(err)
			}
			written, releaseACK := make(chan struct{}), make(chan struct{})
			summaryHeld, releaseSummary := make(chan struct{}), make(chan struct{})
			secondStatus := make(chan struct{})
			unblockACK := sync.OnceFunc(func() { close(releaseACK) })
			unblockSummary := sync.OnceFunc(func() { close(releaseSummary) })
			var afterACK atomic.Bool
			var statuses atomic.Int64
			var holdOnce sync.Once
			disk := editProbeDiskReplies(corpus, root, func(err error) { t.Errorf("fixture read: %v", err) })
			run := newEditProbeStubRun(t, ctx, corpus, plan, root, 1, func(frame stubFrame) (any, *rpcError) {
				args, _ := frame.Params["arguments"].(map[string]any)
				if frame.Method == "tools/call" && frame.Params["name"] == "edit" && args["operation"] == "file" {
					original, err := os.ReadFile(path)
					if err != nil {
						return nil, &rpcError{Code: -32000, Message: err.Error()}
					}
					match, _ := args["match"].(string)
					replacement, _ := args["replacement"].(string)
					if match == "" || strings.Count(string(original), match) != 1 {
						return nil, &rpcError{Code: -32000, Message: "controlled edit match missing or ambiguous"}
					}
					content := strings.Replace(string(original), match, replacement, 1)
					sum := sha256.Sum256([]byte(content))
					if err := os.WriteFile(path, []byte(content), 0600); err != nil {
						return nil, &rpcError{Code: -32000, Message: err.Error()}
					}
					close(written)
					<-releaseACK
					return stubContent(map[string]any{"mutation_receipt": receipt, "readback_exact": true, "status": "applied", "disk_status": "committed", "mutation_id": step.MutationID, "commit_seq": 1, "content_sha256": hex.EncodeToString(sum[:])}), nil
				}
				if frame.Method == "tools/call" && frame.Params["name"] == "change" && args["operation"] == "receipt" {
					if statuses.Add(1) == 2 {
						close(secondStatus)
					}
					if terminalStatus != nil {
						return stubContent(terminalStatus), nil
					}
					return stubContent(map[string]any{"receipt": receipt, "mutation_id": step.MutationID, "graph_status_terminal": true, "graph_status": "fresh"}), nil
				}
				// The fixed editor is connection1; the original independent
				// prober is connection2. Do not block the five editor readbacks.
				if afterACK.Load() && frame.Connection == 2 && frame.Params["name"] == "read" && args["operation"] == "summary" {
					holdOnce.Do(func() { close(summaryHeld) })
					<-releaseSummary
				}
				return disk(frame)
			})
			// accounting_deadline: graph terminal and fresh, every oracle and
			// post-ACK witness present, accounting never complete.
			run.requireAccounting = terminalStatus != nil || mode == "accounting_deadline"
			run.options.Deadline = 2 * time.Second
			probeCtx, stopProbes := context.WithCancel(ctx)
			state := run.activateProbeState(step, stopProbes)
			// A real pre-edit wire request proves original activation ordering.
			initial := editProbeOracles(corpus, step)[0]
			initialRaw, initialID, initialErr := run.perform(ctx, state, state.client, initial.call, "oracle:"+initial.name, "probe", 10)
			if err := run.observeProbe(state, initial, initialID.WriteStarted, initialRaw, initialID, initialErr, false); err != nil {
				t.Fatal(err)
			}
			if initialID.WriteStarted.IsZero() {
				t.Fatal("missing initial probe's wire start")
			}
			// Settlement checks strict timestamp ordering. Give the clock
			// time to advance past the observed probe on coarse-clock hosts
			// before starting the edit and its original call deadline.
			for !time.Now().After(initialID.WriteStarted) {
				if err := waitContext(ctx, time.Millisecond); err != nil {
					t.Fatalf("clock did not advance past pre-edit wire start: %v", err)
				}
			}
			_, callCtx, stopCall, started := run.beginEdit(ctx, step)
			originalDeadline, hasDeadline := callCtx.Deadline()
			if !hasDeadline {
				t.Fatal("missing original bounded call context")
			}
			run.markPending(state)
			type ackResult struct {
				raw      json.RawMessage
				identity wireCallIdentity
				err      error
			}
			ack := make(chan ackResult, 1)
			var workers sync.WaitGroup
			t.Cleanup(func() {
				cancel()
				stopCall()
				stopProbes()
				unblockACK()
				unblockSummary()
				workers.Wait()
				state.stopProbes()
				state.finishRaw()
			})
			workers.Go(func() {
				raw, identity, err := run.perform(callCtx, state, run.editorFor(step), step.Request, "edit", "workload", 0)
				ack <- ackResult{raw, identity, err}
			})
			select {
			case <-written:
			case <-ctx.Done():
				t.Fatal("edit did not write before ACK")
			}
			for index, oracle := range editProbeOracles(corpus, step) {
				raw, identity, err := run.perform(callCtx, state, state.client, oracle.call, "oracle:"+oracle.name, "probe", 10+index)
				if err := run.observeProbe(state, oracle, identity.WriteStarted, raw, identity, err, false); err != nil {
					t.Fatal(err)
				}
			}
			state.mu.Lock()
			allBefore := len(state.record.FirstCorrect) == 7
			for _, value := range state.record.FirstCorrect {
				allBefore = allBefore && value.Value != nil
			}
			state.mu.Unlock()
			if !allBefore {
				t.Fatal("actual pre-ACK fixture responses did not satisfy all original first-correct oracles")
			}
			unblockACK()
			var reply ackResult
			select {
			case reply = <-ack:
			case <-ctx.Done():
				t.Fatal("held ACK did not join")
			}
			if reply.err != nil {
				t.Fatal(reply.err)
			}
			// Deliver the actual ACK before starting the next original probe
			// loop, so the held summary is unambiguously sent post-ACK.
			state.mu.Lock()
			state.acknowledged = reply.identity.Finished
			state.mu.Unlock()
			afterACK.Store(true)
			run.probe(probeCtx, stopProbes, state)
			done := make(chan struct{})
			workers.Go(func() {
				defer close(done)
				defer state.stopProbes()
				defer stopCall()
				defer run.settlePending(state)
				run.finishEditAfterACK(ctx, callCtx, state, step, started, reply.raw, reply.identity, nil)
			})
			select {
			case <-summaryHeld:
			case <-done:
				t.Fatal("collector stopped before held post-ACK summary was observed")
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case <-done:
				t.Fatal("collector finished from pre-ACK FirstCorrect values before required post-ACK coverage")
			case <-secondStatus: // An actual second status proves the old terminal answer did not finish the edit.
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			switch mode {
			case "release", "missing_winner", "accounting_deadline":
				unblockSummary()
			case "cancel":
				cancel()
			case "deadline": // Keep the summary withheld until the existing absolute call context ends.
			}
			select {
			case <-done:
			case <-ctx.Done():
				if mode != "cancel" {
					t.Fatal(ctx.Err())
				}
				<-done
			}
			state.mu.Lock()
			outcome := state.record.Outcome
			state.mu.Unlock()
			want := map[string]string{"release": "complete", "deadline": "product_deadline", "cancel": "phase_censored", "missing_winner": "diagnostic_failure", "accounting_deadline": "product_deadline"}[mode]
			if deadline, ok := callCtx.Deadline(); !ok || deadline != originalDeadline {
				t.Fatal("collector renewed the original deadline")
			}
			if outcome != want {
				t.Fatalf("outcome=%s, want%s", outcome, want)
			}
			// The deadline reason is attribution only: the outcome above and
			// every client time stay as they were.
			state.mu.Lock()
			record := state.record
			state.mu.Unlock()
			wantReason := map[string]string{"deadline": editProbeDeadlineCoverage, "accounting_deadline": editProbeDeadlineAccounting}[mode]
			if record.DeadlineReason != wantReason {
				t.Fatalf("deadline reason=%q, want %q", record.DeadlineReason, wantReason)
			}
			if record.GraphTerminalMS == nil || record.GraphTerminalMS.Value == nil || *record.GraphTerminalMS.Value < 0 {
				t.Fatalf("first terminal status time missing: %+v", record.GraphTerminalMS)
			}
			if mode == "accounting_deadline" {
				relations := editProbeGroupTime(record.FirstCorrect, "relations")
				record.groupTimes()
				if relations.Value == nil || record.Relations.Value == nil || *record.Relations.Value != *relations.Value {
					t.Fatalf("t_relations changed or unavailable: %+v want %+v", record.Relations, relations)
				}
			}
			if mode == "release" || mode == "missing_winner" {
				coverage := editProbeOutputCoverage("gate", run.modeCounts)["json"]
				for tool, observed := range coverage.Tools {
					if observed.ObservedEdits != 1 {
						t.Errorf("post-ACK %s count=%d, want1", tool, observed.ObservedEdits)
					}
				}
				if mode == "missing_winner" {
					state.mu.Lock()
					defer state.mu.Unlock()
					record := state.record
					if record.functionalUnavailableStatus == nil || record.Accounting.State != "unavailable" || record.Accounting.P != nil || record.Accounting.B != nil || len(record.Errors) != 1 {
						t.Fatalf("terminal settlement candidate=%t accounting=%q P_present=%t B_present=%t probe_lead=%s errors=%v",
							record.functionalUnavailableStatus != nil, record.Accounting.State, record.Accounting.P != nil, record.Accounting.B != nil,
							state.editWrite.Sub(state.firstProbeWrite), record.Errors)
					}
				}
			} else if callCtx.Err() == nil {
				t.Fatalf("%s did not retain original context termination", mode)
			}
		})
	}
}
