package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestEditConsumerGCXBusyKeepsNamedWitnessOpen(t *testing.T) {
	const busy = "expensive analysis already running; retry after it completes"
	const dead = `{"dead_code":[],"total":0,"truncated":false}`
	const nativeDead = "GCX1 tool=analyze.dead_code fields=id,kind,name,path,line,reason count=0\n"
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	corpus, plan := editConsumerTestFixture(t)
	plan.Steps[0].ProbeMode = "json"
	step := plan.Steps[0]
	ordinary := editConsumerOracles(corpus)[2]
	gcx := editConsumerGCXOracles([]editProbeOracle{ordinary})[0]
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, corpus.File), []byte("package fixture\n"+corpus.EditAnchor+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ordinaryHeld, releaseOrdinary := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseOrdinary) })
	defer release()
	run := newEditProbeStubRun(t, ctx, corpus, plan, root, 1, func(frame stubFrame) (any, *rpcError) {
		switch frame.Params["name"] {
		case "edit":
			return stubContent(map[string]any{"mutation_id": step.MutationID, "mutation_receipt": "receipt-a", "ack_mode": "local", "disk_status": "committed", "commit_seq": 1}), nil
		case "analyze":
			args, _ := frame.Params["arguments"].(map[string]any)
			output, _ := args["output"].(map[string]any)
			if args["kind"] == "dead_code" && output["format"] == "json" {
				close(ordinaryHeld)
				select {
				case <-releaseOrdinary:
					return outputWire(dead, nil), nil
				case <-ctx.Done():
					return nil, &rpcError{Code: -32000, Message: ctx.Err().Error()}
				}
			}
			if args["kind"] == "dead_code" && output["format"] == "gcx" {
				select {
				case <-releaseOrdinary:
					return outputWire(nativeDead, nil), nil
				default:
					return map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": busy}}}, nil
				}
			}
		}
		return nil, &rpcError{Code: -32602, Message: "unexpected controlled request"}
	})
	run.phase.EditProbe.ConsumerBaseline = &editConsumerBaseline{State: "OBSERVED_DEAD_BEFORE_EDIT", Symbol: corpus.Symbol}
	state, callCtx, stopCall, _ := run.beginEdit(ctx, step)
	defer stopCall()
	deadline, hasDeadline := callCtx.Deadline()
	if !hasDeadline {
		t.Fatal("missing original edit deadline")
	}
	ackRaw, ack, err := run.perform(callCtx, state, run.editorFor(step), step.Request, "edit", "workload", 0)
	ackBody, decodeErr := decodeEditProbeResult(ackRaw)
	if err != nil || decodeErr != nil || ack.Finished.IsZero() ||
		ackBody["mutation_id"] != step.MutationID || ackBody["mutation_receipt"] != "receipt-a" || ackBody["disk_status"] != "committed" {
		t.Fatalf("missing owned fixture ACK: %v / %v / %v", err, decodeErr, ackBody)
	}
	state.acknowledged = ack.Finished
	// Isolate the observed pair. The other named gates are already assessed.
	for name, row := range state.record.Consumers {
		if name != ordinary.name && name != gcx.name {
			row.PostACKAttempts, row.Assessed, row.State = 1, 1, "subject_observed"
			state.record.Consumers[name] = row
		}
	}
	ordinaryDone, ordinaryExited := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(ordinaryExited)
		data, wire, callErr := run.perform(callCtx, state, state.client, ordinary.call, "oracle:"+ordinary.name, "probe", 19)
		ordinaryDone <- errors.Join(callErr, run.observeConsumer(state, ordinary, data, wire, callErr))
	}()
	defer func() {
		release()
		stopCall()
		<-ordinaryExited
	}()
	select {
	case <-ordinaryHeld:
	case <-callCtx.Done():
		t.Fatal("ordinary analysis never held the controlled slot")
	}

	data, first, attempts, callErr := run.performObserved(callCtx, state, state.client, gcx.call, "oracle:"+gcx.name, "probe", 23)
	var refusal *rpcError
	if !errors.As(callErr, &refusal) || refusal.Code != -32000 || refusal.Message != busy ||
		len(attempts) != 1 || attempts[0].Retry != 0 || first.ID == 0 || first.WriteStarted.Before(ack.Finished) {
		t.Fatalf("first post-ACK observation did not retain the exact analysis refusal: %v, %#v", callErr, attempts)
	}
	if err := run.observeConsumer(state, gcx, data, first, callErr); err != nil {
		t.Fatal(err)
	}

	release()
	select {
	case err := <-ordinaryDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-callCtx.Done():
		t.Fatal("ordinary result did not return inside the original deadline")
	}
	if row := state.record.Consumers[ordinary.name]; row.PostACKAttempts != 1 || row.Assessed != 1 || row.State != "fresh_membership_transition" {
		t.Fatalf("ordinary analysis did not return the expected owned transition: %+v", row)
	}
	if row := state.record.Consumers[gcx.name]; row.PostACKAttempts != 1 || row.Errors != 1 || row.Assessed != 0 || row.SilentStale.Value != nil {
		t.Fatalf("first refusal supplied fabricated GCX evidence: %+v", row)
	}
	if editConsumersFinished(state.record.Consumers) {
		t.Error("first busy refusal prematurely finished named witness collection")
	}

	data, later, callErr := run.perform(callCtx, state, state.client, gcx.call, "oracle:"+gcx.name, "probe", 23)
	if callErr != nil || later.ID == 0 || later.ID == first.ID || later.WriteStarted.Before(first.Finished) ||
		later.Finished.After(deadline) || callCtx.Err() != nil {
		t.Fatalf("missing distinct GCX result within the original edit window: %v, first=%+v later=%+v", callErr, first, later)
	}
	if current, _ := callCtx.Deadline(); current != deadline {
		t.Fatal("GCX follow-on extended the original edit deadline")
	}
	if err := run.observeConsumer(state, gcx, data, later, nil); err != nil {
		t.Fatal(err)
	}
	row := state.record.Consumers[gcx.name]
	if row.PostACKAttempts != 2 || row.Errors != 1 || row.Assessed != 1 || row.State != "fresh_membership_transition" ||
		row.SilentStale.Value != nil || !editConsumersFinished(state.record.Consumers) || !editConsumersFailure(state.record.Consumers) {
		t.Fatalf("later native GCX proof or original unavailable/failed accounting was lost: %+v", row)
	}
	state.finishRaw()
	sink := state.sink.(*editProbeHeldSink)
	sink.mu.Lock()
	raw := bytes.Clone(sink.data.Bytes())
	sink.mu.Unlock()
	var retained []string
	var matchedRefusals int
	lines := bufio.NewScanner(bytes.NewReader(raw))
	for lines.Scan() {
		var event struct {
			Event   string          `json:"event"`
			Oracle  string          `json:"oracle"`
			State   string          `json:"state"`
			PostACK bool            `json:"post_ack"`
			Purpose string          `json:"purpose"`
			Tool    string          `json:"tool"`
			Output  string          `json:"output_mode"`
			WireID  int64           `json:"wire_id"`
			WriteNS int64           `json:"write_start_ns"`
			EndNS   int64           `json:"call_end_ns"`
			Payload json.RawMessage `json:"payload"`
		}
		if err := json.Unmarshal(lines.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event.Event == "consumer_probe" && event.Oracle == gcx.name && event.PostACK {
			retained = append(retained, event.State)
		}
		if event.Event == "rpc" && event.Purpose == "oracle:"+gcx.name && event.WireID == first.ID {
			matchedRefusals++
			var payload struct {
				IsError bool `json:"isError"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil ||
				event.Tool != gcx.call.name() || event.Output != "gcx" ||
				event.WriteNS != first.WriteStarted.Sub(run.origin).Nanoseconds() ||
				event.EndNS != first.Finished.Sub(run.origin).Nanoseconds() ||
				!payload.IsError || len(payload.Content) != 1 || payload.Content[0].Type != "text" ||
				payload.Content[0].Text != busy {
				t.Fatalf("first scoped GCX wire payload did not preserve the exact refusal: %v, %s", err, event.Payload)
			}
		}
	}
	if err := lines.Err(); err != nil || len(retained) != 2 || retained[0] != "error" ||
		retained[1] != "fresh_membership_transition" || matchedRefusals != 1 {
		t.Fatalf("raw original and later GCX evidence changed: %v / %v", retained, err)
	}
}

func TestEditConsumerCompletionPreservesAssessedAndUnavailableStates(t *testing.T) {
	corpus, plan := editConsumerTestFixture(t)
	for _, test := range []struct {
		name     string
		row      editConsumerObservation
		finished bool
		failure  bool
	}{
		{"post_ack_error_only", editConsumerObservation{State: "error", PostACKAttempts: 1, Errors: 1, Unassessed: 1}, false, true},
		{"error_after_assessed_answer", editConsumerObservation{State: "error", PostACKAttempts: 2, Assessed: 1, Errors: 1, Unassessed: 1}, true, true},
		{"assessed_pending", editConsumerObservation{State: "pending", PostACKAttempts: 1, Assessed: 1, Pending: 1}, true, false},
		{"assessed_stale", editConsumerObservation{State: "silent_stale", PostACKAttempts: 1, Assessed: 1, stale: 1}, true, true},
		{"unassessable_returned_answer", editConsumerObservation{State: "unassessed", PostACKAttempts: 1, Unassessed: 1}, true, true},
		{"unsupported", editConsumerObservation{State: "unsupported"}, true, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			record := newEditProbeRecord(corpus, plan.Steps[0])
			for name, row := range record.Consumers {
				row.State, row.PostACKAttempts, row.Assessed = "subject_observed", 1, 1
				record.Consumers[name] = row
			}
			record.Consumers["consumer_verify"] = test.row
			if got := editConsumersFinished(record.Consumers); got != test.finished {
				t.Fatalf("named collection finished=%v, want %v", got, test.finished)
			}
			if got := editConsumersFailure(record.Consumers); got != test.failure {
				t.Fatalf("strict observation failed=%v, want %v", got, test.failure)
			}
		})
	}
}
